// Command wasmproxy provides a proxy that rewrites and
// forwards requests by rewriting CORS headers.
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/eandre/wasmproxy/proxy/wasmhttp"
	"github.com/gorilla/websocket"
)

func main() {
	u, _ := url.Parse("https://icanhazdadjoke.com")
	p := NewProxy(u)
	handler := INSECURE_CORS_MIDDLEWARE_FOR_DEVELOPMENT_ONLY(p)
	wasmhttp.Serve(handler)
	select {}
}

func INSECURE_CORS_MIDDLEWARE_FOR_DEVELOPMENT_ONLY(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		origin := req.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, PATCH, PUT, OPTIONS, TRACE")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Sentry-Trace")
		w.Header().Set("Access-Control-Allow-Credentials", "true")

		if req.Method == "OPTIONS" {
			return
		}

		h.ServeHTTP(w, req)
	})
}

func NewProxy(baseURL *url.URL) *Proxy {
	up := &websocket.Upgrader{
		CheckOrigin: func(req *http.Request) bool {
			return true
		},
	}

	// Use the standard reverse proxy, but filter out any CORS headers
	// from upstream as we're setting them ourselves.
	rp := httputil.NewSingleHostReverseProxy(baseURL)
	orig := rp.ModifyResponse
	rp.ModifyResponse = func(resp *http.Response) error {
		if orig != nil {
			if err := orig(resp); err != nil {
				return err
			}
		}

		h := resp.Header
		h.Del("Access-Control-Allow-Credentials")
		h.Del("Access-Control-Allow-Headers")
		h.Del("Access-Control-Allow-Methods")
		h.Del("Access-Control-Allow-Origin")
		return nil
	}

	return &Proxy{
		baseURL:  baseURL,
		upgrader: up,
		rp:       rp,
	}
}

type Proxy struct {
	baseURL  *url.URL
	upgrader *websocket.Upgrader
	rp       *httputil.ReverseProxy
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	log.Printf("proxying request to %s", req.URL.Path)
	if strings.Contains(strings.ToLower(req.Header.Get("Upgrade")), "websocket") {
		// Looks like a websocket
		p.proxyWebsocket(w, req)
		return
	}
	p.rp.ServeHTTP(w, req)
}

// The websocket proxy implementation below is adapted from
// github.com/koding/websocketproxy.
//
// The MIT License (MIT)
//
// Copyright (c) 2014 Koding, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
// of the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED,
// INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR
// A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT
// HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION
// OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE
// SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

func (p *Proxy) proxyWebsocket(w http.ResponseWriter, req *http.Request) {
	backendURL := p.rewrite(req.URL)
	// Convert http(s) to ws(s)
	switch backendURL.Scheme {
	case "http":
		backendURL.Scheme = "ws"
	case "https":
		backendURL.Scheme = "wss"
	}

	// Pass headers from the incoming request to the dialer to forward them to
	// the final destinations.
	requestHeader := http.Header{}
	for _, prot := range req.Header[http.CanonicalHeaderKey("Sec-WebSocket-Protocol")] {
		requestHeader.Add("Sec-WebSocket-Protocol", prot)
	}
	for _, cookie := range req.Header[http.CanonicalHeaderKey("Cookie")] {
		requestHeader.Add("Cookie", cookie)
	}
	if req.Host != "" {
		requestHeader.Set("Host", req.Host)
	}

	// Pass X-Forwarded-For headers too, code below is a part of
	// httputil.ReverseProxy. See http://en.wikipedia.org/wiki/X-Forwarded-For
	// for more information
	// TODO: use RFC7239 http://tools.ietf.org/html/rfc7239
	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := req.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		requestHeader.Set("X-Forwarded-For", clientIP)
	}

	// Set the originating protocol of the incoming HTTP request. The SSL might
	// be terminated on our site and because we doing proxy adding this would
	// be helpful for applications on the backend.
	requestHeader.Set("X-Forwarded-Proto", "http")
	if req.TLS != nil {
		requestHeader.Set("X-Forwarded-Proto", "https")
	}

	// Connect to the backend URL, also pass the headers we get from the requst
	// together with the Forwarded headers we prepared above.
	// TODO: support multiplexing on the same backend connection instead of
	// opening a new TCP connection time for each request. This should be
	// optional:
	// http://tools.ietf.org/html/draft-ietf-hybi-websocket-multiplexing-01
	connBackend, resp, err := websocket.DefaultDialer.Dial(backendURL.String(), requestHeader)
	if err != nil {
		log.Printf("wasmproxy: couldn't dial %s: %v", backendURL, err)
		if resp != nil {
			// If the WebSocket handshake fails, ErrBadHandshake is returned
			// along with a non-nil *http.Response so that callers can handle
			// redirects, authentication, etcetera.
			if err := copyResponse(w, resp); err != nil {
				log.Printf("wasmproxy: couldn't write response after failed remote backend handshake: %s", err)
			}
		} else {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		}
		return
	}
	defer connBackend.Close()

	// Only pass those headers to the upgrader.
	upgradeHeader := http.Header{}
	if hdr := resp.Header.Get("Sec-Websocket-Protocol"); hdr != "" {
		upgradeHeader.Set("Sec-Websocket-Protocol", hdr)
	}
	if hdr := resp.Header.Get("Set-Cookie"); hdr != "" {
		upgradeHeader.Set("Set-Cookie", hdr)
	}

	// Now upgrade the existing incoming request to a WebSocket connection.
	// Also pass the header that we gathered from the Dial handshake.
	connPub, err := p.upgrader.Upgrade(w, req, upgradeHeader)
	if err != nil {
		log.Printf("wasmproxy: couldn't upgrade %s", err)
		return
	}
	defer connPub.Close()

	errClient := make(chan error, 1)
	errBackend := make(chan error, 1)
	replicateWebsocketConn := func(dst, src *websocket.Conn, errc chan error) {
		for {
			msgType, msg, err := src.ReadMessage()
			if err != nil {
				m := websocket.FormatCloseMessage(websocket.CloseNormalClosure, fmt.Sprintf("%v", err))
				if e, ok := err.(*websocket.CloseError); ok {
					if e.Code != websocket.CloseNoStatusReceived {
						m = websocket.FormatCloseMessage(e.Code, e.Text)
					}
				}
				errc <- err
				dst.WriteMessage(websocket.CloseMessage, m)
				break
			}
			err = dst.WriteMessage(msgType, msg)
			if err != nil {
				errc <- err
				break
			}
		}
	}

	go replicateWebsocketConn(connPub, connBackend, errClient)
	go replicateWebsocketConn(connBackend, connPub, errBackend)

	var message string
	select {
	case err = <-errClient:
		message = "wasmproxy: error when copying from backend to client: %v"
	case err = <-errBackend:
		message = "wasmproxy: error when copying from client to backend: %v"
	}
	if _, ok := err.(*websocket.CloseError); !ok {
		log.Printf(message, err)
	}
}

// rewrite rewrites an URL to the backend we want to connect to.
func (p *Proxy) rewrite(u *url.URL) *url.URL {
	u2 := *p.baseURL // copy
	u2.Fragment = u.Fragment
	u2.Path = u.Path
	u2.RawQuery = u.RawQuery
	return &u2
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func copyResponse(rw http.ResponseWriter, resp *http.Response) error {
	copyHeader(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	defer resp.Body.Close()
	_, err := io.Copy(rw, resp.Body)
	return err
}
