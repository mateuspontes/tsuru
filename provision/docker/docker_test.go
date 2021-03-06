// Copyright 2013 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package docker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsouza/go-dockerclient"
	"github.com/fsouza/go-dockerclient/testing"
	"github.com/tsuru/config"
	"github.com/tsuru/docker-cluster/cluster"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/app/image"
	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/event"
	"github.com/tsuru/tsuru/net"
	"github.com/tsuru/tsuru/permission"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/provision/docker/container"
	"github.com/tsuru/tsuru/provision/docker/types"
	"github.com/tsuru/tsuru/provision/provisiontest"
	"github.com/tsuru/tsuru/router/routertest"
	"github.com/tsuru/tsuru/safe"
	"gopkg.in/check.v1"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

type newContainerOpts struct {
	AppName         string
	Status          string
	Image           string
	ProcessName     string
	ImageCustomData map[string]interface{}
	Provisioner     *dockerProvisioner
}

func (s *S) newContainer(opts *newContainerOpts, p *dockerProvisioner) (*container.Container, error) {
	container := container.Container{
		Container: types.Container{
			ID:          "id",
			IP:          "10.10.10.10",
			HostPort:    "3333",
			HostAddr:    "127.0.0.1",
			ProcessName: "web",
			ExposedPort: "8888/tcp",
		},
	}
	if p == nil {
		p = s.p
	}
	imageName := "tsuru/python:latest"
	var customData map[string]interface{}
	if opts != nil {
		if opts.Image != "" {
			imageName = opts.Image
		}
		container.AppName = opts.AppName
		container.ProcessName = opts.ProcessName
		customData = opts.ImageCustomData
		if opts.Provisioner != nil {
			p = opts.Provisioner
		}
		container.SetStatus(p, provision.Status(opts.Status), false)
	}
	err := s.newFakeImage(p, imageName, customData)
	if err != nil {
		return nil, err
	}
	if container.AppName == "" {
		container.AppName = "container"
	}
	routertest.FakeRouter.AddBackend(container.AppName)
	routertest.FakeRouter.AddRoute(container.AppName, container.Address())
	ports := map[docker.Port]struct{}{
		docker.Port(s.port + "/tcp"): {},
	}
	config := docker.Config{
		Image:        imageName,
		Cmd:          []string{"ps"},
		ExposedPorts: ports,
	}
	createOptions := docker.CreateContainerOptions{Config: &config}
	createOptions.Name = randomString()
	_, c, err := p.Cluster().CreateContainer(createOptions, net.StreamInactivityTimeout)
	if err != nil {
		return nil, err
	}
	container.ID = c.ID
	container.Image = imageName
	container.Name = createOptions.Name
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	err = conn.Collection(s.collName).Insert(&container)
	if err != nil {
		return nil, err
	}
	imageId, err := image.AppCurrentImageName(container.AppName)
	if err != nil {
		return nil, err
	}
	err = s.newFakeImage(p, imageId, nil)
	if err != nil {
		return nil, err
	}
	return &container, nil
}

func (s *S) removeTestContainer(c *container.Container) error {
	routertest.FakeRouter.RemoveBackend(c.AppName)
	return c.Remove(s.p)
}

func (s *S) newFakeImage(p *dockerProvisioner, repo string, customData map[string]interface{}) error {
	if customData == nil {
		customData = map[string]interface{}{
			"processes": map[string]interface{}{
				"web": "python myapp.py",
			},
		}
	}
	var buf safe.Buffer
	opts := docker.PullImageOptions{Repository: repo, OutputStream: &buf}
	err := image.SaveImageCustomData(repo, customData)
	if err != nil && !mgo.IsDup(err) {
		return err
	}
	return p.Cluster().PullImage(opts, docker.AuthConfiguration{})
}

func (s *S) TestGetContainer(c *check.C) {
	coll := s.p.Collection()
	defer coll.Close()
	coll.Insert(
		container.Container{Container: types.Container{ID: "abcdef", Type: "python"}},
		container.Container{Container: types.Container{ID: "fedajs", Type: "ruby"}},
		container.Container{Container: types.Container{ID: "wat", Type: "java"}},
	)
	defer coll.RemoveAll(bson.M{"id": bson.M{"$in": []string{"abcdef", "fedajs", "wat"}}})
	container, err := s.p.GetContainer("abcdef")
	c.Assert(err, check.IsNil)
	c.Assert(container.ID, check.Equals, "abcdef")
	c.Assert(container.Type, check.Equals, "python")
	container, err = s.p.GetContainer("wut")
	c.Assert(container, check.IsNil)
	c.Assert(err, check.NotNil)
	e, ok := err.(*provision.UnitNotFoundError)
	c.Assert(ok, check.Equals, true)
	c.Assert(e.ID, check.Equals, "wut")
}

func (s *S) TestGetContainers(c *check.C) {
	coll := s.p.Collection()
	defer coll.Close()
	coll.Insert(
		container.Container{Container: types.Container{ID: "abcdef", Type: "python", AppName: "something"}},
		container.Container{Container: types.Container{ID: "fedajs", Type: "python", AppName: "something"}},
		container.Container{Container: types.Container{ID: "wat", Type: "java", AppName: "otherthing"}},
	)
	defer coll.RemoveAll(bson.M{"id": bson.M{"$in": []string{"abcdef", "fedajs", "wat"}}})
	containers, err := s.p.listContainersByApp("something")
	c.Assert(err, check.IsNil)
	c.Assert(containers, check.HasLen, 2)
	ids := []string{containers[0].ID, containers[1].ID}
	sort.Strings(ids)
	c.Assert(ids[0], check.Equals, "abcdef")
	c.Assert(ids[1], check.Equals, "fedajs")
	containers, err = s.p.listContainersByApp("otherthing")
	c.Assert(err, check.IsNil)
	c.Assert(containers, check.HasLen, 1)
	c.Assert(containers[0].ID, check.Equals, "wat")
	containers, err = s.p.listContainersByApp("unknown")
	c.Assert(err, check.IsNil)
	c.Assert(containers, check.HasLen, 0)
}

func (s *S) TestGetImageFromAppPlatform(c *check.C) {
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	img := image.GetBuildImage(app)
	repoNamespace, err := config.GetString("docker:repository-namespace")
	c.Assert(err, check.IsNil)
	c.Assert(img, check.Equals, fmt.Sprintf("%s/python:latest", repoNamespace))
}

func (s *S) TestGetImageAppWhenDeployIsMultipleOf10(c *check.C) {
	app := &app.App{Name: "app1", Platform: "python", Deploys: 20}
	err := s.storage.Apps().Insert(app)
	c.Assert(err, check.IsNil)
	cont := container.Container{Container: types.Container{ID: "bleble", Type: app.Platform, AppName: app.Name, Image: "tsuru/app1"}}
	coll := s.p.Collection()
	err = coll.Insert(cont)
	c.Assert(err, check.IsNil)
	defer coll.Close()
	c.Assert(err, check.IsNil)
	defer coll.RemoveAll(bson.M{"id": cont.ID})
	img := image.GetBuildImage(app)
	repoNamespace, err := config.GetString("docker:repository-namespace")
	c.Assert(err, check.IsNil)
	c.Assert(img, check.Equals, fmt.Sprintf("%s/%s:latest", repoNamespace, app.Platform))
}

func (s *S) TestGetImageWithRegistry(c *check.C) {
	config.Set("docker:registry", "localhost:3030")
	defer config.Unset("docker:registry")
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	img := image.GetBuildImage(app)
	repoNamespace, _ := config.GetString("docker:repository-namespace")
	expected := fmt.Sprintf("localhost:3030/%s/python:latest", repoNamespace)
	c.Assert(img, check.Equals, expected)
}

func (s *S) TestArchiveDeploy(c *check.C) {
	stopCh := s.stopContainers(s.server.URL(), 1)
	defer func() { <-stopCh }()
	err := s.newFakeImage(s.p, "tsuru/python:latest", nil)
	c.Assert(err, check.IsNil)
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	routertest.FakeRouter.AddBackend(app.GetName())
	defer routertest.FakeRouter.RemoveBackend(app.GetName())
	img, err := s.p.archiveDeploy(app, image.GetBuildImage(app), "https://s3.amazonaws.com/wat/archive.tar.gz", nil)
	c.Assert(err, check.IsNil)
	c.Assert(img, check.Equals, "tsuru/app-myapp:v1")
}

func (s *S) TestArchiveDeployCanceledEvent(c *check.C) {
	err := s.newFakeImage(s.p, "tsuru/python:latest", nil)
	c.Assert(err, check.IsNil)
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	routertest.FakeRouter.AddBackend(app.GetName())
	defer routertest.FakeRouter.RemoveBackend(app.GetName())
	evt, err := event.New(&event.Opts{
		Target:        event.Target{Type: "app", Value: "myapp"},
		Kind:          permission.PermAppDeploy,
		Owner:         s.token,
		Cancelable:    true,
		Allowed:       event.Allowed(permission.PermApp),
		AllowedCancel: event.Allowed(permission.PermApp),
	})
	c.Assert(err, check.IsNil)
	done := make(chan bool)
	go func() {
		defer close(done)
		img, depErr := s.p.archiveDeploy(app, image.GetBuildImage(app), "https://s3.amazonaws.com/wat/archive.tar.gz", evt)
		c.Assert(depErr, check.ErrorMatches, "deploy canceled by user action")
		c.Assert(img, check.Equals, "")
	}()
	time.Sleep(100 * time.Millisecond)
	err = evt.TryCancel("because yes", "majortom@ground.control")
	c.Assert(err, check.IsNil)
	<-done
}

func (s *S) TestArchiveDeployRegisterRace(c *check.C) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(10))
	var p dockerProvisioner
	var registerCount int64
	server, err := testing.NewServer("127.0.0.1:0", nil, func(r *http.Request) {
		go func(path string) {
			parts := strings.Split(path, "/")
			if len(parts) == 4 && parts[3] == "start" {
				registerErr := p.RegisterUnit(nil, parts[2], nil)
				if registerErr == nil {
					atomic.AddInt64(&registerCount, 1)
				}
			}
		}(r.URL.Path)
	})
	c.Assert(err, check.IsNil)
	defer server.Stop()
	config.Set("docker:registry", "localhost:3030")
	defer config.Unset("docker:registry")
	err = p.Initialize()
	c.Assert(err, check.IsNil)
	p.cluster, err = cluster.New(nil, &cluster.MapStorage{},
		cluster.Node{Address: server.URL()})
	c.Assert(err, check.IsNil)
	err = s.newFakeImage(&p, "tsuru/python:latest", nil)
	c.Assert(err, check.IsNil)
	nTests := 100
	stopCh := s.stopContainers(server.URL(), uint(nTests))
	defer func() { <-stopCh }()
	wg := sync.WaitGroup{}
	for i := 0; i < nTests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("myapp-%d", i)
			app := provisiontest.NewFakeApp(name, "python", 1)
			routertest.FakeRouter.AddBackend(app.GetName())
			defer routertest.FakeRouter.RemoveBackend(app.GetName())
			img, _ := p.archiveDeploy(app, image.GetBuildImage(app), "https://s3.amazonaws.com/wat/archive.tar.gz", nil)
			c.Assert(img, check.Equals, "localhost:3030/tsuru/app-"+name+":v1")
		}(i)
	}
	wg.Wait()
	c.Assert(registerCount, check.Equals, int64(nTests))
}

func (s *S) TestStart(c *check.C) {
	err := s.newFakeImage(s.p, "tsuru/python:latest", nil)
	c.Assert(err, check.IsNil)
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	imageId := image.GetBuildImage(app)
	routertest.FakeRouter.AddBackend(app.GetName())
	defer routertest.FakeRouter.RemoveBackend(app.GetName())
	var buf bytes.Buffer
	cont, err := s.p.start(&container.Container{Container: types.Container{ProcessName: "web"}}, app, imageId, &buf, "")
	c.Assert(err, check.IsNil)
	defer cont.Remove(s.p)
	c.Assert(cont.ID, check.Not(check.Equals), "")
	cont2, err := s.p.GetContainer(cont.ID)
	c.Assert(err, check.IsNil)
	c.Assert(cont2.Image, check.Equals, imageId)
	c.Assert(cont2.Status, check.Equals, provision.StatusStarting.String())
}

func (s *S) TestStartStoppedContainer(c *check.C) {
	cont, err := s.newContainer(nil, nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	cont.Status = provision.StatusStopped.String()
	err = s.newFakeImage(s.p, "tsuru/python:latest", nil)
	c.Assert(err, check.IsNil)
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	imageId := image.GetBuildImage(app)
	routertest.FakeRouter.AddBackend(app.GetName())
	defer routertest.FakeRouter.RemoveBackend(app.GetName())
	var buf bytes.Buffer
	cont, err = s.p.start(cont, app, imageId, &buf, "")
	c.Assert(err, check.IsNil)
	defer cont.Remove(s.p)
	c.Assert(cont.ID, check.Not(check.Equals), "")
	cont2, err := s.p.GetContainer(cont.ID)
	c.Assert(err, check.IsNil)
	c.Assert(cont2.Image, check.Equals, imageId)
	c.Assert(cont2.Status, check.Equals, provision.StatusStopped.String())
}

func (s *S) TestProvisionerGetCluster(c *check.C) {
	config.Set("docker:cluster:redis-server", "127.0.0.1:6379")
	defer config.Unset("docker:cluster:redis-server")
	var p dockerProvisioner
	err := p.Initialize()
	c.Assert(err, check.IsNil)
	clus := p.Cluster()
	c.Assert(clus, check.NotNil)
	currentNodes, err := clus.Nodes()
	c.Assert(err, check.IsNil)
	c.Assert(currentNodes, check.HasLen, 0)
	c.Assert(p.scheduler, check.NotNil)
}

func (s *S) TestPushImage(c *check.C) {
	var requests []*http.Request
	server, err := testing.NewServer("127.0.0.1:0", nil, func(r *http.Request) {
		requests = append(requests, r)
	})
	c.Assert(err, check.IsNil)
	defer server.Stop()
	config.Set("docker:registry", "localhost:3030")
	defer config.Unset("docker:registry")
	var p dockerProvisioner
	err = p.Initialize()
	c.Assert(err, check.IsNil)
	p.cluster, err = cluster.New(nil, &cluster.MapStorage{},
		cluster.Node{Address: server.URL()})
	c.Assert(err, check.IsNil)
	err = s.newFakeImage(&p, "localhost:3030/base/img", nil)
	c.Assert(err, check.IsNil)
	err = p.PushImage("localhost:3030/base/img", "")
	c.Assert(err, check.IsNil)
	c.Assert(requests, check.HasLen, 3)
	c.Assert(requests[0].URL.Path, check.Equals, "/images/create")
	c.Assert(requests[1].URL.Path, check.Equals, "/images/localhost:3030/base/img/json")
	c.Assert(requests[2].URL.Path, check.Equals, "/images/localhost:3030/base/img/push")
	c.Assert(requests[2].URL.RawQuery, check.Equals, "")
	err = s.newFakeImage(&p, "localhost:3030/base/img:v2", nil)
	c.Assert(err, check.IsNil)
	err = p.PushImage("localhost:3030/base/img", "v2")
	c.Assert(err, check.IsNil)
	c.Assert(requests, check.HasLen, 6)
	c.Assert(requests[3].URL.Path, check.Equals, "/images/create")
	c.Assert(requests[4].URL.Path, check.Equals, "/images/localhost:3030/base/img:v2/json")
	c.Assert(requests[5].URL.Path, check.Equals, "/images/localhost:3030/base/img/push")
	c.Assert(requests[5].URL.RawQuery, check.Equals, "tag=v2")
}

func (s *S) TestPushImageAuth(c *check.C) {
	var requests []*http.Request
	server, err := testing.NewServer("127.0.0.1:0", nil, func(r *http.Request) {
		requests = append(requests, r)
	})
	c.Assert(err, check.IsNil)
	defer server.Stop()
	config.Set("docker:registry", "localhost:3030")
	config.Set("docker:registry-auth:email", "me@company.com")
	config.Set("docker:registry-auth:username", "myuser")
	config.Set("docker:registry-auth:password", "mypassword")
	defer config.Unset("docker:registry")
	var p dockerProvisioner
	err = p.Initialize()
	c.Assert(err, check.IsNil)
	p.cluster, err = cluster.New(nil, &cluster.MapStorage{},
		cluster.Node{Address: server.URL()})
	c.Assert(err, check.IsNil)
	err = s.newFakeImage(&p, "localhost:3030/base/img", nil)
	c.Assert(err, check.IsNil)
	err = p.PushImage("localhost:3030/base/img", "")
	c.Assert(err, check.IsNil)
	c.Assert(requests, check.HasLen, 3)
	c.Assert(requests[0].URL.Path, check.Equals, "/images/create")
	c.Assert(requests[1].URL.Path, check.Equals, "/images/localhost:3030/base/img/json")
	c.Assert(requests[2].URL.Path, check.Equals, "/images/localhost:3030/base/img/push")
	c.Assert(requests[2].URL.RawQuery, check.Equals, "")
	auth := requests[2].Header.Get("X-Registry-Auth")
	var providedAuth docker.AuthConfiguration
	data, err := base64.StdEncoding.DecodeString(auth)
	c.Assert(err, check.IsNil)
	err = json.Unmarshal(data, &providedAuth)
	c.Assert(err, check.IsNil)
	c.Assert(providedAuth.ServerAddress, check.Equals, "localhost:3030")
	c.Assert(providedAuth.Email, check.Equals, "me@company.com")
	c.Assert(providedAuth.Username, check.Equals, "myuser")
	c.Assert(providedAuth.Password, check.Equals, "mypassword")
}

func (s *S) TestPushImageNoRegistry(c *check.C) {
	var request *http.Request
	server, err := testing.NewServer("127.0.0.1:0", nil, func(r *http.Request) {
		request = r
	})
	c.Assert(err, check.IsNil)
	defer server.Stop()
	err = s.p.PushImage("localhost:3030/base", "")
	c.Assert(err, check.IsNil)
	c.Assert(request, check.IsNil)
}

func (s *S) TestBuildClusterStorage(c *check.C) {
	defer config.Set("docker:cluster:mongo-url", "127.0.0.1:27017")
	defer config.Set("docker:cluster:mongo-database", "docker_provision_tests_cluster_stor")
	config.Unset("docker:cluster:mongo-url")
	_, err := buildClusterStorage()
	c.Assert(err, check.ErrorMatches, ".*docker:cluster:{mongo-url,mongo-database} must be set.")
	config.Set("docker:cluster:mongo-url", "127.0.0.1:27017")
	config.Unset("docker:cluster:mongo-database")
	_, err = buildClusterStorage()
	c.Assert(err, check.ErrorMatches, ".*docker:cluster:{mongo-url,mongo-database} must be set.")
	config.Set("docker:cluster:storage", "xxxx")
}

func (s *S) TestGetNodeByHost(c *check.C) {
	var p dockerProvisioner
	err := p.Initialize()
	c.Assert(err, check.IsNil)
	nodes := []cluster.Node{{
		Address: "http://h1:80",
	}, {
		Address: "http://h2:90",
	}, {
		Address: "http://h3",
	}, {
		Address: "h4",
	}, {
		Address: "h5:30123",
	}}
	p.cluster, err = cluster.New(nil, &cluster.MapStorage{}, nodes...)
	c.Assert(err, check.IsNil)
	tests := [][]string{
		{"h1", nodes[0].Address},
		{"h2", nodes[1].Address},
		{"h3", nodes[2].Address},
		{"h4", nodes[3].Address},
		{"h5", nodes[4].Address},
	}
	for _, t := range tests {
		var n cluster.Node
		n, err = p.GetNodeByHost(t[0])
		c.Assert(err, check.IsNil)
		c.Assert(n.Address, check.DeepEquals, t[1])
	}
	_, err = p.GetNodeByHost("h6")
	c.Assert(err, check.ErrorMatches, `node with host "h6" not found`)
}
