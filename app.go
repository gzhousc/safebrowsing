// Copyright 2016 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command sbserver is an application for serving URL lookups via a simple API.
//
// In order to abstract away the complexities of the Safe Browsing API v4, the
// sbserver application can be used to serve a subset of API v4 over HTTP.
// This subset is intentionally small so that it would be easy to implement by
// a client. It is intended for sbserver to either be running locally on a
// client's machine or within the same local network. That way it can handle
// most local API calls before resorting to making an API call to the actual
// Safe Browsing API over the internet.
//
// Usage of sbserver looks something like this:
//	             _________________
//	            |                 |
//	            |  Safe Browsing  |
//	            |  API v4 servers |
//	            |_________________|
//	                     |
//	            ~ ~ ~ ~ ~ ~ ~ ~ ~ ~
//	               The Internet
//	            ~ ~ ~ ~ ~ ~ ~ ~ ~ ~
//	                     |
//	              _______V_______
//	             |               |
//	             |   SBServer    |
//	      +------|  Application  |------+
//	      |      |_______________|      |
//	      |              |              |
//	 _____V_____    _____V_____    _____V_____
//	|           |  |           |  |           |
//	|  Client1  |  |  Client2  |  |  Client3  |
//	|___________|  |___________|  |___________|
//
// In theory, each client could directly use the Go SafeBrowser implementation,
// but there are situations where that is not desirable. For example, the client
// may not be using the language Go, or there may be multiple clients in the
// same machine or local network that would like to share a local database
// and cache. The sbserver was designed to address these issues.
//
// The sbserver application is technically a proxy since it is itself actually
// an API v4 client. It connects the Safe Browsing API servers using an API key
// and maintains a local database and cache. However, it is also a server since
// it re-serves a subset of the API v4 endpoints. These endpoints are minimal
// in that they do not require each client to maintain state between calls.
//
// The assumption is that communication between SBServer and Client1, Client2,
// and Client3 is inexpensive, since they are within the same machine or same
// local network. Thus, the sbserver can efficiently satisfy some requests
// without talking to the global Safe Browsing servers since it has a
// potentially larger cache. Furthermore, it can multiplex multiple requests to
// the Safe Browsing servers on fewer TCP connections, reducing the cost for
// comparatively more expensive internet transfers.
//
// By default, the sbserver listens on localhost:8080 and serves the following
// API endpoints:
//	/v4/threatMatches:find
//	/v4/threatLists
//	/status
//	/r
//
//
// Endpoint: /v4/threatMatches:find
//
// This is a lightweight implementation of the API v4 threatMatches endpoint.
// Essentially, it takes in a list of URLs, and returns a list of threat matches
// for those URLs. Unlike the Safe Browsing API, it does not require an API key.
//
// Example usage:
//	# Send request to server:
//	$ curl \
//	  -H "Content-Type: application/json" \
//	  -X POST -d '{
//	      "threatInfo": {
//	          "threatTypes":      ["UNWANTED_SOFTWARE", "MALWARE"],
//	          "platformTypes":    ["ANY_PLATFORM"],
//	          "threatEntryTypes": ["URL"],
//	          "threatEntries": [
//	              {"url": "google.com"},
//	              {"url": "bad1url.org"},
//	              {"url": "bad2url.org"}
//	          ]
//	      }
//	  }' \
//	  localhost:8080/v4/threatMatches:find
//
//	# Receive response from server:
//	{
//	    "matches": [{
//	        "threat":          {"url": "bad1url.org"},
//	        "platformType":    "ANY_PLATFORM",
//	        "threatType":      "UNWANTED_SOFTWARE",
//	        "threatEntryType": "URL"
//	    }, {
//	        "threat":          {"url": "bad2url.org"},
//	        "platformType":    "ANY_PLATFORM",
//	        "threatType":      "UNWANTED_SOFTWARE",
//	        "threatEntryType": "URL"
//	    }, {
//	        "threat":          {"url": "bad2url.org"},
//	        "platformType":    "ANY_PLATFORM",
//	        "threatType":      "MALWARE",
//	        "threatEntryType": "URL"
//	    }]
//	}
//
//
// Endpoint: /v4/threatLists
//
// The endpoint returns a list of the threat lists that the sbserver is
// currently subscribed to. The threats returned by the earlier threatMatches
// API call may only be one of these types.
//
// Example usage:
//	# Send request to server:
//	$ curl -X GET localhost:8080/v4/threatLists
//
//	# Receive response from server:
//	{
//	    "threatLists": [{
//	        "threatType":      "MALWARE"
//	        "platformType":    "ANY_PLATFORM",
//	        "threatEntryType": "URL",
//	    }, {
//	        "threatType":      "SOCIAL_ENGINEERING",
//	        "platformType":    "ANY_PLATFORM"
//	        "threatEntryType": "URL",
//	    }, {
//	        "threatType":      "UNWANTED_SOFTWARE"
//	        "platformType":    "ANY_PLATFORM",
//	        "threatEntryType": "URL",
//	    }]
//	}
//
//
// Endpoint: /status
//
// The status endpoint allows a client to obtain some statistical information
// regarding the health of sbserver. It can be used to determine how many
// requests were satisfied locally by sbserver alone and how many requests
// were forwarded to the Safe Browsing API servers.
//
// Example usage:
//	$ curl localhost:8080/status
//	{
//	    "Stats" : {
//	        "QueriesByDatabase" : 132,
//	        "QueriesByCache" : 31,
//	        "QueriesByAPI" : 6,
//	        "QueriesFail" : 0,
//	    },
//	    "Error" : ""
//	}
//
//
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	//"net/url"
	"os"

	"github.com/google/safebrowsing"
	pb "./safebrowsing/safebrowsing_proto"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	_ "github.com/google/safebrowsing/cmd/sbserver/statik"
	"github.com/rakyll/statik/fs"
)

const (
	statusPath         = "/status"
	findThreatPath     = "/v4/threatMatches:find"
	getThreatListsPath = "/v4/threatLists"
	gaeHealthCheckPath = "/_ah/health"
)

const (
	mimeJSON  = "application/json"
	mimeProto = "application/x-protobuf"
)

var (
	apiKeyFlag   = flag.String("apikey", "", "specify your Safe Browsing API key")
	srvAddrFlag  = flag.String("srvaddr", "localhost:8080", "TCP network address the HTTP server should use")
	databaseFlag = flag.String("db", "", "path to the Safe Browsing database.")
)

const usage = `sbserver: starts a Safe Browsing API proxy server.

In order to abstract away the complexities of the Safe Browsing API v4, the
sbserver application can be used to serve a subset of the v4 API.
This subset is intentionally small so that it would be easy to implement by
a client. It is intended for sbserver to either be running locally on a
client's machine or within the same local network so that it can handle most
local API calls before resorting to making an API call to the actual
Safe Browsing API over the internet.

Usage: %s -apikey=$APIKEY

`

// unmarshal reads pbResp from req. The mime will either be JSON or ProtoBuf.
func unmarshal(req *http.Request, pbReq proto.Message) (string, error) {
	var mime string
	alt := req.URL.Query().Get("alt")
	if alt == "" {
		alt = req.Header.Get("Content-Type")
	}
	switch alt {
	case "json", mimeJSON:
		mime = mimeJSON
	case "proto", mimeProto:
		mime = mimeProto
	default:
		return mime, errors.New("invalid interchange format")
	}

	switch req.Header.Get("Content-Type") {
	case mimeJSON:
		if err := jsonpb.Unmarshal(req.Body, pbReq); err != nil {
			return mime, err
		}
	case mimeProto:
		body, err := ioutil.ReadAll(req.Body)
		if err != nil {
			return mime, err
		}
		if err := proto.Unmarshal(body, pbReq); err != nil {
			return mime, err
		}
	}
	return mime, nil
}

// marshal writes pbResp into resp. The mime can either be JSON or ProtoBuf.
func marshal(resp http.ResponseWriter, pbResp proto.Message, mime string) error {
	resp.Header().Set("Content-Type", mime)
	switch mime {
	case mimeProto:
		body, err := proto.Marshal(pbResp)
		if err != nil {
			return err
		}
		if _, err := resp.Write(body); err != nil {
			return err
		}
	case mimeJSON:
		var m jsonpb.Marshaler
		var b bytes.Buffer
		if err := m.Marshal(&b, pbResp); err != nil {
			return err
		}
		if _, err := resp.Write(b.Bytes()); err != nil {
			return err
		}
	default:
		return errors.New("invalid interchange format")
	}
	return nil
}

// serveStatus writes a simple JSON with server status information to resp.
func serveStatus(resp http.ResponseWriter, req *http.Request, sb *safebrowsing.SafeBrowser) {
	stats, sbErr := sb.Status()
	errStr := ""
	if sbErr != nil {
		errStr = sbErr.Error()
	}
	buf, err := json.Marshal(struct {
		Stats safebrowsing.Stats
		Error string
	}{stats, errStr})
	if err != nil {
		http.Error(resp, err.Error(), http.StatusInternalServerError)
		return
	}
	resp.Header().Set("Content-Type", mimeJSON)
	resp.Write(buf)
}

// serveLookups is a light-weight implementation of the "/v4/threatMatches:find"
// API endpoint. This allows clients to look up whether a given URL is safe.
// Unlike the official API, it does not require an API key.
// It supports both JSON and ProtoBuf.
func serveLookups(resp http.ResponseWriter, req *http.Request, sb *safebrowsing.SafeBrowser) {
	if req.Method != "POST" {
		http.Error(resp, "invalid method", http.StatusBadRequest)
		return
	}

	// Decode the request message.
	pbReq := new(pb.FindThreatMatchesRequest)
	mime, err := unmarshal(req, pbReq)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}

	// TODO: Should this handler use the information in threatTypes,
	// platformTypes, and threatEntryTypes?

	// Parse the request message.
	var urls []string
	tes := pbReq.GetThreatInfo().GetThreatEntries()
	for _, u := range tes {
		urls = append(urls, u.Url)
		if u.Url == "" || len(u.Hash) > 0 {
			http.Error(resp, "only ThreatEntry.Url may be set", http.StatusBadRequest)
			return
		}
	}

	// Lookup the URLs.
	utss, err := sb.LookupURLs(urls)
	if err != nil {
		http.Error(resp, err.Error(), http.StatusInternalServerError)
		return
	}

	// Compose the response message.
	pbResp := new(pb.FindThreatMatchesResponse)
	for i, uts := range utss {
		// Use map to condense duplicate ThreatDescriptor entries.
		tdm := make(map[safebrowsing.ThreatDescriptor]bool)
		for _, ut := range uts {
			tdm[ut.ThreatDescriptor] = true
		}

		for td := range tdm {
			tm := &pb.ThreatMatch{
				Threat:          &pb.ThreatEntry{Url: urls[i]},
				ThreatType:      pb.ThreatType(td.ThreatType),
				PlatformType:    pb.PlatformType(td.PlatformType),
				ThreatEntryType: pb.ThreatEntryType(td.ThreatEntryType),
			}
			pbResp.Matches = append(pbResp.Matches, tm)
		}
	}

	// Encode the response message.
	if err := marshal(resp, pbResp, mime); err != nil {
		http.Error(resp, err.Error(), http.StatusInternalServerError)
		return
	}
}

// serveLists is a light-weight implementation of the "/v4/threatLists"
// API endpoint. This informs the client of which threat lists are available.
// Unlike the official API, it does not require an API key.
// It supports both JSON and ProtoBuf.
func serveLists(resp http.ResponseWriter, req *http.Request, conf *safebrowsing.Config) {
	var mime string
	switch req.URL.Query().Get("alt") {
	case "", "json":
		mime = mimeJSON
	case "proto":
		mime = mimeProto
	default:
		http.Error(resp, "invalid request type", http.StatusBadRequest)
		return
	}
	if req.Method != "GET" {
		http.Error(resp, "invalid method", http.StatusBadRequest)
		return
	}

	tls := safebrowsing.DefaultThreatLists
	if len(conf.ThreatLists) != 0 {
		tls = conf.ThreatLists
	}

	pbResp := new(pb.ListThreatListsResponse)
	for _, td := range tls {
		pbResp.ThreatLists = append(pbResp.ThreatLists, &pb.ThreatListDescriptor{
			ThreatType:      pb.ThreatType(td.ThreatType),
			PlatformType:    pb.PlatformType(td.PlatformType),
			ThreatEntryType: pb.ThreatEntryType(td.ThreatEntryType),
		})
	}

	// Encode the response message.
	if err := marshal(resp, pbResp, mime); err != nil {
		http.Error(resp, err.Error(), http.StatusInternalServerError)
		return
	}
}

func parseTemplates(fs http.FileSystem, t *template.Template, paths ...string) (*template.Template, error) {
	for _, path := range paths {
		file, err := fs.Open(path)
		if err != nil {
			return nil, err
		}
		tmpl, err := ioutil.ReadAll(file)
		if err != nil {
			return nil, err
		}
		t, err = t.Parse(string(tmpl))
		if err != nil {
			return nil, err
		}
	}
	return t, nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, usage, os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *apiKeyFlag == "" {
		fmt.Fprintln(os.Stderr, "No -apikey specified")
		os.Exit(1)
	}
	conf := safebrowsing.Config{
		APIKey: *apiKeyFlag,
		DBPath: *databaseFlag,
		Logger: os.Stderr,
	}
	sb, err := safebrowsing.NewSafeBrowser(conf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Unable to initialize Safe Browsing client: ", err)
		os.Exit(1)
	}
	statikFS, err := fs.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Unable to initialize static files: ", err)
		os.Exit(1)
	}

	http.HandleFunc(gaeHealthCheckPath, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	http.HandleFunc(statusPath, func(w http.ResponseWriter, r *http.Request) {
		serveStatus(w, r, sb)
	})
	http.HandleFunc(findThreatPath, func(w http.ResponseWriter, r *http.Request) {
		serveLookups(w, r, sb)
	})
	http.HandleFunc(getThreatListsPath, func(w http.ResponseWriter, r *http.Request) {
		serveLists(w, r, &conf)
	})
	http.Handle("/public/", http.StripPrefix("/public/", http.FileServer(statikFS)))

	fmt.Fprintln(os.Stdout, "Starting server at", *srvAddrFlag)
	if err := http.ListenAndServe(*srvAddrFlag, nil); err != nil {
		fmt.Fprintln(os.Stderr, "Server error:", err)
		return
	}
	fmt.Fprintln(os.Stdout, "Stopping server")
}
