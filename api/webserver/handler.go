package webserver

import (
	"github.com/timeredbull/tsuru/api/auth"
	"net/http"
)

type Handler func(http.ResponseWriter, *http.Request) error

func (fn Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := fn(w, r); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

type AuthorizationRequiredHandler func(http.ResponseWriter, *http.Request, *auth.User) error

func (fn AuthorizationRequiredHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("Authorization")
	if token == "" {
		http.Error(w, "You must provide the Authorization header", http.StatusBadRequest)
	} else if user, err := auth.CheckToken(token); err != nil {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
	} else if err = fn(w, r, user); err != nil {
		code := http.StatusInternalServerError
		if e, ok := err.(*HttpError); ok {
			code = e.code
		}
		http.Error(w, err.Error(), code)
	}
}
