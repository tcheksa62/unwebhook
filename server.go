package main

import (
	"bytes"
	"encoding/json"
	"github.com/dimfeld/glog"
	"github.com/dimfeld/httptreemux"
	"net"
	"net/http"
)

type HookHandler func(http.ResponseWriter, *http.Request, map[string]string, *Hook)

func hookHandler(w http.ResponseWriter, r *http.Request, params map[string]string, hook *Hook) {
	gitlabEventType := r.Header.Get("X-Gitlab-Event")

	if r.ContentLength > 16384 {
		// We should never get a request this large.
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}

	buffer := bytes.Buffer{}
	buffer.ReadFrom(r.Body)
	r.Body.Close()

	if glog.V(2) {
		niceBuffer := &bytes.Buffer{}
		json.Indent(niceBuffer, buffer.Bytes(), "", "  ")
		glog.Infof("Hook %s received data %s\n",
			r.URL.Path, string(niceBuffer.Bytes()))
	}

	if hook.Secret != "" {
		if r.Header.Get("X-Gitlab-Token") != "" {
			secret := r.Header.Get("X-Gitlab-Token")
			if secret != hook.Secret {
				glog.Warningf("Request with bad secret for hook %s from %s [%s]",
					r.URL.Path, r.RemoteAddr, secret)
				w.WriteHeader(http.StatusForbidden)
				return
			}
		} else {
			glog.Warningf("Request with no secret for hook %s from %s\n",
				r.URL.Path, r.RemoteAddr)
			w.WriteHeader(http.StatusForbidden)
			return
		}
	}

	event, err := NewEvent(buffer.Bytes(), gitlabEventType)
	if err != nil {
		glog.Errorf("Error parinsg JSON for %s: %s", r.URL.Path, err)
		return
	}
	event["urlparams"] = params
	go hook.Execute(event)
}

func handlerWrapper(handler HookHandler, hook *Hook) httptreemux.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, params map[string]string) {
		glog.Infoln("Called", r.URL.Path)
		handler(w, r, params, hook)
	}
}

func SetupServer(config *Config) (net.Listener, http.Handler) {
	var listener net.Listener = nil

	listener, err := net.Listen("tcp", config.ListenAddress)
	if err != nil {
		glog.Fatalf("Could not listen on %s: %s\n", config.ListenAddress, err)
	}

	if len(config.AcceptIps) != 0 {
		listenFilter := NewListenFilter(listener, WhiteList)
		for _, a := range config.AcceptIps {
			glog.Infoln("Adding IP filter", a)
			listenFilter.AddString(a)
		}
		listener = listenFilter
	}

	router := httptreemux.New()

	for _, hook := range config.Hook {
		router.POST(hook.Url, handlerWrapper(hookHandler, hook))
	}

	return listener, router
}

func RunServer(config *Config) {
	listener, router := SetupServer(config)
	glog.Fatal(http.Serve(listener, router))
}
