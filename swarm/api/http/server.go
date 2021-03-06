// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

/*
A simple http server interface to Swarm
*/
package http

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/swarm/api"
	"github.com/ethereum/go-ethereum/swarm/storage"
	"github.com/rs/cors"
)

const (
	rawType = "application/octet-stream"
)

var (
	// accepted protocols: bzz (traditional), bzzi (immutable) and bzzr (raw)
	bzzPrefix       = regexp.MustCompile("^/+bzz[ir]?:/+")
	trailingSlashes = regexp.MustCompile("/+$")
	rootDocumentUri = regexp.MustCompile("^/+bzz[i]?:/+[^/]+$")
	// forever         = func() time.Time { return time.Unix(0, 0) }
	forever = time.Now
)

type sequentialReader struct {
	reader io.Reader
	pos    int64
	ahead  map[int64](chan bool)
	lock   sync.Mutex
}

// ServerConfig is the basic configuration needed for the HTTP server and also
// includes CORS settings.
type ServerConfig struct {
	Addr       string
	CorsString string
}

// browser API for registering bzz url scheme handlers:
// https://developer.mozilla.org/en/docs/Web-based_protocol_handlers
// electron (chromium) api for registering bzz url scheme handlers:
// https://github.com/atom/electron/blob/master/docs/api/protocol.md

// starts up http server
func StartHttpServer(api *api.Api, config *ServerConfig) {
	var allowedOrigins []string
	for _, domain := range strings.Split(config.CorsString, ",") {
		allowedOrigins = append(allowedOrigins, strings.TrimSpace(domain))
	}
	c := cors.New(cors.Options{
		AllowedOrigins: allowedOrigins,
		AllowedMethods: []string{"POST", "GET", "DELETE", "PATCH", "PUT"},
		MaxAge:         600,
		AllowedHeaders: []string{"*"},
	})
	hdlr := c.Handler(NewServer(api))

	go http.ListenAndServe(config.Addr, hdlr)
	log.Info(fmt.Sprintf("Swarm HTTP proxy started on localhost:%s", config.Addr))
}

func NewServer(api *api.Api) *Server {
	return &Server{api}
}

type Server struct {
	api *api.Api
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestURL := r.URL
	// This is wrong
	//	if requestURL.Host == "" {
	//		var err error
	//		requestURL, err = url.Parse(r.Referer() + requestURL.String())
	//		if err != nil {
	//			http.Error(w, err.Error(), http.StatusBadRequest)
	//			return
	//		}
	//	}
	log.Debug(fmt.Sprintf("HTTP %s request URL: '%s', Host: '%s', Path: '%s', Referer: '%s', Accept: '%s'", r.Method, r.RequestURI, requestURL.Host, requestURL.Path, r.Referer(), r.Header.Get("Accept")))
	uri := requestURL.Path
	var raw, nameresolver bool
	var proto string

	// HTTP-based URL protocol handler
	log.Debug(fmt.Sprintf("BZZ request URI: '%s'", uri))

	path := bzzPrefix.ReplaceAllStringFunc(uri, func(p string) string {
		proto = p
		return ""
	})

	// protocol identification (ugly)
	if proto == "" {
		log.Error(fmt.Sprintf("[BZZ] Swarm: Protocol error in request `%s`.", uri))
		http.Error(w, "Invalid request URL: need access protocol (bzz:/, bzzr:/, bzzi:/) as first element in path.", http.StatusBadRequest)
		return
	}
	if len(proto) > 4 {
		raw = proto[1:5] == "bzzr"
		nameresolver = proto[1:5] != "bzzi"
	}

	log.Debug("", "msg", log.Lazy{Fn: func() string {
		return fmt.Sprintf("[BZZ] Swarm: %s request over protocol %s '%s' received.", r.Method, proto, path)
	}})

	switch {
	case r.Method == "POST" || r.Method == "PUT":
		if r.Header.Get("content-length") == "" {
			http.Error(w, "Missing Content-Length header in request.", http.StatusBadRequest)
			return
		}
		key, err := s.api.Store(io.LimitReader(r.Body, r.ContentLength), r.ContentLength, nil)
		if err == nil {
			log.Debug(fmt.Sprintf("Content for %v stored", key.Log()))
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Method == "POST" {
			if raw {
				w.Header().Set("Content-Type", "text/plain")
				http.ServeContent(w, r, "", time.Now(), bytes.NewReader([]byte(common.Bytes2Hex(key))))
			} else {
				http.Error(w, "No POST to "+uri+" allowed.", http.StatusBadRequest)
				return
			}
		} else {
			// PUT
			if raw {
				http.Error(w, "No PUT to /raw allowed.", http.StatusBadRequest)
				return
			} else {
				path = api.RegularSlashes(path)
				mime := r.Header.Get("Content-Type")
				// TODO proper root hash separation
				log.Debug(fmt.Sprintf("Modify '%s' to store %v as '%s'.", path, key.Log(), mime))
				newKey, err := s.api.Modify(path, common.Bytes2Hex(key), mime, nameresolver)
				if err == nil {
					log.Debug(fmt.Sprintf("Swarm replaced manifest by '%s'", newKey))
					w.Header().Set("Content-Type", "text/plain")
					http.ServeContent(w, r, "", time.Now(), bytes.NewReader([]byte(newKey)))
				} else {
					http.Error(w, "PUT to "+path+"failed.", http.StatusBadRequest)
					return
				}
			}
		}
	case r.Method == "DELETE":
		if raw {
			http.Error(w, "No DELETE to /raw allowed.", http.StatusBadRequest)
			return
		} else {
			path = api.RegularSlashes(path)
			log.Debug(fmt.Sprintf("Delete '%s'.", path))
			newKey, err := s.api.Modify(path, "", "", nameresolver)
			if err == nil {
				log.Debug(fmt.Sprintf("Swarm replaced manifest by '%s'", newKey))
				w.Header().Set("Content-Type", "text/plain")
				http.ServeContent(w, r, "", time.Now(), bytes.NewReader([]byte(newKey)))
			} else {
				http.Error(w, "DELETE to "+path+"failed.", http.StatusBadRequest)
				return
			}
		}
	case r.Method == "GET" || r.Method == "HEAD":
		path = trailingSlashes.ReplaceAllString(path, "")
		if path == "" {
			http.Error(w, "Empty path not allowed", http.StatusBadRequest)
			return
		}
		if raw {
			var reader storage.LazySectionReader
			parsedurl, _ := api.Parse(path)

			if parsedurl == path {
				key, err := s.api.Resolve(parsedurl, nameresolver)
				if err != nil {
					log.Error(fmt.Sprintf("%v", err))
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				reader = s.api.Retrieve(key)
			} else {
				var status int
				readertmp, _, status, err := s.api.Get(path, nameresolver)
				if err != nil {
					http.Error(w, err.Error(), status)
					return
				}
				reader = readertmp
			}

			// retrieving content

			quitC := make(chan bool)
			size, err := reader.Size(quitC)
			if err != nil {
				log.Debug(fmt.Sprintf("Could not determine size: %v", err.Error()))
				//An error on call to Size means we don't have the root chunk
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			log.Debug(fmt.Sprintf("Reading %d bytes.", size))

			// setting mime type
			qv := requestURL.Query()
			mimeType := qv.Get("content_type")
			if mimeType == "" {
				mimeType = rawType
			}

			w.Header().Set("Content-Type", mimeType)
			http.ServeContent(w, r, uri, forever(), reader)
			log.Debug(fmt.Sprintf("Serve raw content '%s' (%d bytes) as '%s'", uri, size, mimeType))

			// retrieve path via manifest
		} else {
			log.Debug(fmt.Sprintf("Structured GET request '%s' received.", uri))
			// add trailing slash, if missing
			if rootDocumentUri.MatchString(uri) {
				http.Redirect(w, r, path+"/", http.StatusFound)
				return
			}
			reader, mimeType, status, err := s.api.Get(path, nameresolver)
			if err != nil {
				if _, ok := err.(api.ErrResolve); ok {
					log.Debug(fmt.Sprintf("%v", err))
					status = http.StatusBadRequest
				} else {
					log.Debug(fmt.Sprintf("error retrieving '%s': %v", uri, err))
					status = http.StatusNotFound
				}
				http.Error(w, err.Error(), status)
				return
			}
			// set mime type and status headers
			w.Header().Set("Content-Type", mimeType)
			if status > 0 {
				w.WriteHeader(status)
			} else {
				status = 200
			}
			quitC := make(chan bool)
			size, err := reader.Size(quitC)
			if err != nil {
				log.Debug(fmt.Sprintf("Could not determine size: %v", err.Error()))
				//An error on call to Size means we don't have the root chunk
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			log.Debug(fmt.Sprintf("Served '%s' (%d bytes) as '%s' (status code: %v)", uri, size, mimeType, status))

			http.ServeContent(w, r, path, forever(), reader)

		}
	default:
		http.Error(w, "Method "+r.Method+" is not supported.", http.StatusMethodNotAllowed)
	}
}

func (self *sequentialReader) ReadAt(target []byte, off int64) (n int, err error) {
	self.lock.Lock()
	// assert self.pos <= off
	if self.pos > off {
		log.Error(fmt.Sprintf("non-sequential read attempted from sequentialReader; %d > %d", self.pos, off))
		panic("Non-sequential read attempt")
	}
	if self.pos != off {
		log.Debug(fmt.Sprintf("deferred read in POST at position %d, offset %d.", self.pos, off))
		wait := make(chan bool)
		self.ahead[off] = wait
		self.lock.Unlock()
		if <-wait {
			// failed read behind
			n = 0
			err = io.ErrUnexpectedEOF
			return
		}
		self.lock.Lock()
	}
	localPos := 0
	for localPos < len(target) {
		n, err = self.reader.Read(target[localPos:])
		localPos += n
		log.Debug(fmt.Sprintf("Read %d bytes into buffer size %d from POST, error %v.", n, len(target), err))
		if err != nil {
			log.Debug(fmt.Sprintf("POST stream's reading terminated with %v.", err))
			for i := range self.ahead {
				self.ahead[i] <- true
				delete(self.ahead, i)
			}
			self.lock.Unlock()
			return localPos, err
		}
		self.pos += int64(n)
	}
	wait := self.ahead[self.pos]
	if wait != nil {
		log.Debug(fmt.Sprintf("deferred read in POST at position %d triggered.", self.pos))
		delete(self.ahead, self.pos)
		close(wait)
	}
	self.lock.Unlock()
	return localPos, err
}
