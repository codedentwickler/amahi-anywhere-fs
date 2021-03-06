/*
 * Copyright (c) 2013-2018 Amahi
 *
 * This file is part of Amahi.
 *
 * Amahi is free software released under the GNU GPL v3 license.
 * See the LICENSE file accompanying this distribution.
 */

package main

import (
	"bytes"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"github.com/amahi/go-metadata"
	"github.com/gorilla/mux"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"golang.org/x/net/http2"
)

const HEADER_END = "\n"

type FSError struct {
	message string
}

func (err *FSError) Error() string {
	return err.message
}

// MercuryFsService defines the file server and directory server API
type MercuryFsService struct {
	Shares *HdaShares
	Apps   *HdaApps

	// TLS configuration
	TLSConfig *tls.Config

	// http server hooks
	server *http.Server

	info *HdaInfo

	metadata *metadata.Library

	debug_info *debugInfo

	api_router *mux.Router
}

// NewMercuryFsService creates a new MercuryFsService, sets the FileDirectoryRoot
// and CurrentDirectory to rootDirectory and returns the pointer to the
// newly created MercuryFsService
func NewMercuryFSService(root_dir, local_addr string) (service *MercuryFsService, err error) {
	service = new(MercuryFsService)

	service.Shares, err = NewHdaShares(root_dir)
	if err != nil {
		debug(3, "Error making HdaShares: %s", err.Error())
		return nil, err
	}
	service.debug_info = new(debugInfo)

	// set up API mux
	api_router := mux.NewRouter()
	api_router.HandleFunc("/shares", service.serve_shares).Methods("GET")
	api_router.HandleFunc("/files", service.serve_file).Methods("GET")
	api_router.HandleFunc("/files", service.delete_file).Methods("DELETE")
	api_router.HandleFunc("/files", service.upload_file).Methods("POST")
	api_router.HandleFunc("/apps", service.apps_list).Methods("GET")
	api_router.HandleFunc("/md", service.get_metadata).Methods("GET")
	api_router.HandleFunc("/hda_debug", service.hda_debug).Methods("GET")

	service.api_router = api_router

	mux := http.NewServeMux()
	mux.HandleFunc("/", http.HandlerFunc(service.top_vhost_filter))

	service.server = &http.Server{TLSConfig: service.TLSConfig, Handler:mux}

	service.info = new(HdaInfo)
	service.info.version = VERSION
	if local_addr != "" {
		service.info.local_addr = local_addr
	} else {

		actual_addr, err := GetLocalAddr(root_dir)
		if err != nil {
			debug(2, "Error getting local address: %s", err.Error())
			return nil, err
		}
		service.info.local_addr = actual_addr + ":" + LOCAL_SERVER_PORT
	}
	// This will be set when the HDA connects to the proxy
	service.info.relay_addr = ""

	debug(3, "Amahi FS Service started %s", service.Shares.to_json())
	debug(4, "HDA Info: %s", service.info.to_json())

	return service, err
}

// String returns FileDirectoryRoot and CurrentDirectory with a newline between them
func (service *MercuryFsService) String() string {
	// TODO: Possibly change this to present a more formatted string
	return service.Shares.to_json()
}

func (service *MercuryFsService) hda_debug(writer http.ResponseWriter, request *http.Request) {
	// I am purposely not calling any of the update methods of debugInfo to actually provide valuable info
	result := "{\n"
	result += fmt.Sprintf("\"goroutines\": %d\n", runtime.NumGoroutine())
	relay_addr := service.info.relay_addr
	result += `"connected": `
	if relay_addr != "" {
		result += "true\n"
	} else {
		result += "false\n"
	}
	last, received, served, num_bytes := service.debug_info.everything()
	actualDate := ""
	if served != 0 {
		actualDate = last.Format(http.TimeFormat)
	}
	outstanding := received - served
	if outstanding < 0 {
		outstanding = 0
	}
	result += fmt.Sprintf("\"last_request\": \"%s\"\n", actualDate)
	result += fmt.Sprintf("\"received\": %d\n", received)
	result += fmt.Sprintf("\"served\": %d\n", served)
	result += fmt.Sprintf("\"outstanding\": %d\n", outstanding)
	result += fmt.Sprintf("\"bytes_served\": %d\n", num_bytes)

	result += "}"
	writer.WriteHeader(200)
	writer.Write([]byte(result))
}

func directory(fi os.FileInfo, js string, w http.ResponseWriter, request *http.Request) (status, size int64) {
	json := []byte(js)
	etag := `"` + sha1bytes(json) + `"`
	w.Header().Set("ETag", etag)
	inm := request.Header.Get("If-None-Match")
	if inm == etag {
		size = 0
		debug(4, "If-None-Match match found for %s", etag)
		w.WriteHeader(http.StatusNotModified)
		status = 304
	} else {
		debug(4, "If-None-Match (%s) match NOT found for Etag %s", inm, etag)
		size = int64(len(json))
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("Last-Modified", fi.ModTime().UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=0, private, must-revalidate")
		w.WriteHeader(http.StatusOK)
		w.Write(json)
		status = 200
	}
	return status, size
}

// fullPathToFile creates the full path to the requested file and checks to make sure that
// there aren't any  '..' to prevent unauthorized access
func (service *MercuryFsService) fullPathToFile(shareName, relativePath string) (string, error) {
	share := service.Shares.Get(shareName)

	if share == nil {
		return "", errors.New(fmt.Sprintf("Share %s not found", shareName))
	} else if strings.Contains(relativePath, "../") {
		return "", errors.New(fmt.Sprintf("path %s contains ..", relativePath))
	}

	path := share.Path() + relativePath
	debug(3, "Full path: %s", path)
	return path, nil
}

// serve requests with the ServeConn function over HTTP/2, in goroutines, until we get some error
func (service *MercuryFsService) StartServing(conn net.Conn) error {
	log("Connection to the proxy established.")

	service.info.relay_addr = conn.RemoteAddr().String()

	serveConnOpts := &http2.ServeConnOpts{BaseConfig: service.server}
	server2 := new(http2.Server)

	// start serving over http2 on provided conn and block until connection is lost
	server2.ServeConn(conn, serveConnOpts)

	log("Lost connection to the proxy.")
	service.info.relay_addr = ""

	return errors.New("connection is no longer readable")
}

func (service *MercuryFsService) serve_file(writer http.ResponseWriter, request *http.Request) {
	q := request.URL
	path := q.Query().Get("p")
	share := q.Query().Get("s")
	ua := request.Header.Get("User-Agent")
	query := pathForLog(request.URL)

	debug(2, "serve_file GET request")

	service.print_request(request)

	full_path, err := service.fullPathToFile(share, path)
	if err != nil {
		debug(2, "File not found: %s", err)
		http.NotFound(writer, request)
		service.debug_info.requestServed(int64(0))
		log("\"GET %s\" 404 0 \"%s\"", query, ua)
		return
	}
	osFile, err := os.Open(full_path)
	if err != nil {
		debug(2, "Error opening file: %s", err.Error())
		http.NotFound(writer, request)
		service.debug_info.requestServed(int64(0))
		log("\"GET %s\" 404 0 \"%s\"", query, ua)
		return
	}
	defer osFile.Close()

	// This shouldn't return an error since we just opened the file
	fi, _ := osFile.Stat()

	// If the file is a directory, return the all the files within the directory...
	if fi.IsDir() || isSymlinkDir(fi, full_path) {
		jsonDir, err := dirToJSON(osFile, full_path)
		if err != nil {
			debug(2, "Error converting dir to JSON: %s", err.Error())
			log("\"GET %s\" 404 0 \"%s\"", query, ua)
			http.NotFound(writer, request)
			service.debug_info.requestServed(int64(0))
			return
		}
		debug(5, "%s", jsonDir)
		status, size := directory(fi, jsonDir, writer, request)
		service.debug_info.requestServed(size)
		log("\"GET %s\" %d %d \"%s\"", query, status, size, ua)
		return
	}

	// we use for etag the sha1sum of the full path followed the mtime
	mtime := fi.ModTime().UTC().Format(http.TimeFormat)
	etag := `"`+sha1string(path+mtime)+`"`
	inm := request.Header.Get("If-None-Match")
	if inm == etag {
		debug(4, "If-None-Match match found for %s", etag)
		writer.WriteHeader(http.StatusNotModified)
		log("\"GET %s\" %d \"%s\"", query, 304, ua)
	} else {
		writer.Header().Set("Last-Modified", mtime)
		writer.Header().Set("ETag", etag)
		writer.Header().Set("Cache-Control", "max-age=0, private, must-revalidate")
		debug(4, "Etag sent: %s", etag)
		http.ServeContent(writer, request, full_path, fi.ModTime(), osFile)
		log("\"GET %s\" %d %d \"%s\"", query, 200, fi.Size(), ua)
		service.debug_info.requestServed(fi.Size())
	}

	return
}

func (service *MercuryFsService) serve_shares(writer http.ResponseWriter, request *http.Request) {
	service.Shares.update_shares()
	debug(5, "========= DEBUG Share request: %d", len(service.Shares.Shares))
	json := service.Shares.to_json()
	debug(5, "Share JSON: %s", json)
	etag := `"` + sha1bytes([]byte(json)) + `"`
	inm := request.Header.Get("If-None-Match")
	if inm == etag {
		debug(4, "If-None-Match match found for %s", etag)
		writer.WriteHeader(http.StatusNotModified)
		service.debug_info.requestServed(int64(0))
	} else {
		debug(4, "If-None-Match (%s) match NOT found for Etag %s", inm, etag)
		size := int64(len(json))
		writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		writer.Header().Set("Last-Modified", service.Shares.LastChecked.Format(http.TimeFormat))
		writer.Header().Set("ETag", etag)
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "max-age=0, private, must-revalidate")
		writer.WriteHeader(http.StatusOK)
		writer.Write([]byte(json))
		service.debug_info.requestServed(size)
	}
}

func GetLocalAddr(root_dir string) (string, error) {

	if root_dir != "" {
		return "127.0.0.1", nil
	}

	dbconn, err := sql.Open("mysql", MYSQL_CREDENTIALS)
	if err != nil {
		log(err.Error())
		return "", err
	}
	defer dbconn.Close()

	var prefix, addr string
	q := "SELECT value FROM settings WHERE name=\"net\""
	row := dbconn.QueryRow(q)
	err = row.Scan(&prefix)
	if err != nil {
		log(err.Error())
		return "", err
	}

	q = "SELECT value FROM settings WHERE name=\"self-address\""
	row = dbconn.QueryRow(q)
	err = row.Scan(&addr)
	if err != nil {
		log("Error scanning self-address: %s\n", err.Error())
		return "", err
	}

	debug(4, "prefix: %s\taddr: %s", prefix, addr)
	return prefix + "." + addr, nil
}

func pathForLog(u *url.URL) string {
	var buf bytes.Buffer
	buf.WriteString(u.Path)
	if u.RawQuery != "" {
		buf.WriteByte('?')
		buf.WriteString(u.RawQuery)
	}
	if u.Fragment != "" {
		buf.WriteByte('#')
		buf.WriteString(url.QueryEscape(u.Fragment))
	}
	return buf.String()
}

func isSymlinkDir(m os.FileInfo, fullpath string) bool {
	// debug(1, "isSymlinkDir(%s)", m.Name())
	// not a symlink, so return
	if m.Mode()&os.ModeSymlink == 0 {
		// debug(1, "isSymlink: not a symlink")
		return false
	}
	// it's a symlink, is the destination a directory?
	linkedpath := fullpath + "/" + m.Name()
	// debug(1, "isSymlink: %s - %s / %s", fullpath, filepath.Dir(fullpath), m.Name())
	link, err := os.Readlink(linkedpath)
	if err != nil {
		// debug(1, "isSymlink: error reading symlink: %s", err)
		return false
	}
	// default to absolute path
	dest := link
	if link[0] != '/' {
		// if not starting in /, it's a relative path
		dest = fullpath + "/" + link
	}
	// debug(1, "isSymlink: symlink is: %s", dest)
	file, err := os.Open(dest)
	if err != nil {
		// debug(1, "isSymlink: error opening symlink destination: %s", err)
		return false
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil {
		// debug(1, "isSymlink: error in stat: %s", err)
		return false
	}
	// debug(1, "isSymlink: info: %s", fi)
	return fi.IsDir()
}

func (service *MercuryFsService) apps_list(writer http.ResponseWriter, request *http.Request) {
	apps, err := newHdaApps()
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	service.Apps = apps
	service.Apps.list()
	debug(5, "========= DEBUG apps_list request: %d", len(service.Shares.Shares))
	json := service.Apps.to_json()
	debug(5, "App JSON: %s", json)
	etag := `"` + sha1bytes([]byte(json)) + `"`
	inm := request.Header.Get("If-None-Match")
	if inm == etag {
		debug(4, "If-None-Match match found for %s", etag)
		writer.WriteHeader(http.StatusNotModified)
		service.debug_info.requestServed(int64(0))
	} else {
		debug(4, "If-None-Match (%s) match NOT found for Etag %s", inm, etag)
		size := int64(len(json))
		writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		writer.Header().Set("ETag", etag)
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "max-age=0, private, must-revalidate")
		writer.WriteHeader(http.StatusOK)
		writer.Write([]byte(json))
		service.debug_info.requestServed(size)
	}
}

func (service *MercuryFsService) get_metadata(writer http.ResponseWriter, request *http.Request) {
	// get the filename and the hint
	q := request.URL
	filename, err := url.QueryUnescape(q.Query().Get("f"))
	if err != nil {
		debug(3, "get_metadata error parsing file: %s", err)
		http.NotFound(writer, request)
		return
	}
	hint, err := url.QueryUnescape(q.Query().Get("h"))
	if err != nil {
		debug(3, "get_metadata error parsing hint: %s", err)
		http.NotFound(writer, request)
		return
	}
	debug(5, "metadata filename: %s", filename)
	debug(5, "metadata hint: %s", hint)
	// FIXME
	json, err := service.metadata.GetMetadata(filename, hint)
	if err != nil {
		debug(3, "metadata error: %s", err)
		http.NotFound(writer, request)
		return
	}
	debug(5, "========= DEBUG get_metadata request: %d", len(service.Shares.Shares))
	debug(5, "metadata JSON: %s", json)
	etag := `"` + sha1bytes([]byte(json)) + `"`
	inm := request.Header.Get("If-None-Match")
	if inm == etag {
		debug(4, "If-None-Match match found for %s", etag)
		writer.WriteHeader(http.StatusNotModified)
		service.debug_info.requestServed(int64(0))
	} else {
		debug(4, "If-None-Match (%s) match NOT found for Etag %s", inm, etag)
		size := int64(len(json))
		writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		writer.Header().Set("ETag", etag)
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "max-age=0, private, must-revalidate")
		writer.WriteHeader(http.StatusOK)
		writer.Write([]byte(json))
		service.debug_info.requestServed(size)
	}
}

func (service *MercuryFsService) print_request(request *http.Request) {
	debug(5, "REQUEST [from %s] BEGIN =========================", request.RemoteAddr)
	if (request.Method != "POST") {
		raw_request, _ := httputil.DumpRequest(request, true)
		debug(5, "%s", raw_request)
	} else {
		debug(5, "POST Request to %s (details removed)", request.URL)
	}
	debug(5, "REQUEST END =========================")
}

func (service *MercuryFsService) top_vhost_filter(writer http.ResponseWriter, request *http.Request) {

	header := writer.Header()

	ua := request.Header.Get("User-Agent")
	// since data will change with the session, we should indicate that to keep caching!
	header.Add("Vary", "Session")
	if ua == "" {
		service.print_request(request)
		// if no UA, it's an API call
		service.api_router.ServeHTTP(writer, request)
		return
	}

	// search for vhost
	re := regexp.MustCompile(`Vhost/([^\s]*)`)
	matches := re.FindStringSubmatch(ua)
	debug(5, "VHOST matches %q *************************", matches)
	if len(matches) != 2 {
		service.print_request(request)
		// if no vhost, default to API?
		service.api_router.ServeHTTP(writer, request)
		return
	}

	service.print_request(request)

	vhost := matches[1]

	debug(5, "VHOST REQUEST FOR %s *************************", vhost)

	request.URL.Host = "hda"
	request.Host = vhost

	// FIXME - support https and other ports later
	remote, err := url.Parse("http://" + vhost)
	if err != nil {
		debug(5, "REQUEST ERROR: %s", err)
		http.NotFound(writer, request)
		return
	}

	// proxy the app request
	proxy := httputil.NewSingleHostReverseProxy(remote)
	// since data will change with the UA, we should indicate that to keep caching!
	header.Add("Vary", "User-Agent")
	proxy.ServeHTTP(writer, request)
}

// delete a file!
func (service *MercuryFsService) delete_file(writer http.ResponseWriter, request *http.Request) {
	q := request.URL
	path := q.Query().Get("p")
	share := q.Query().Get("s")
	ua := request.Header.Get("User-Agent")
	query := pathForLog(request.URL)

	debug(2, "delete_file DELETE request")

	service.print_request(request)

	full_path, err := service.fullPathToFile(share, path)

	// if using the welcome server, just return OK without deleting anything
	if (!no_delete) {
		if err != nil {
			debug(2, "File not found: %s", err)
			http.NotFound(writer, request)
			service.debug_info.requestServed(int64(0))
			log("\"DELETE %s\" 404 0 \"%s\"", query, ua)
			return
		}
		err = os.Remove(full_path)
		if err != nil {
			debug(2, "Error removing file: %s", err.Error())
			writer.WriteHeader(http.StatusExpectationFailed)
			service.debug_info.requestServed(int64(0))
			log("\"DELETE %s\" 417 0 \"%s\"", query, ua)
			return
		}
	}	else {
		debug(2, "NOTICE: Running in no-delete mode. Would have deleted: %s", full_path)
	}

	writer.WriteHeader(http.StatusOK)

	return
}

// upload a file!
func (service *MercuryFsService) upload_file(writer http.ResponseWriter, request *http.Request) {
	q := request.URL
	path := q.Query().Get("p")
	share := q.Query().Get("s")
	ua := request.Header.Get("User-Agent")
	query := pathForLog(request.URL)

	debug(2, "upload_file POST request")

	// do NOT print the whole request, as an image may be way way too big
	service.print_request(request)

	// full_path, err := service.fullPathToFile(share, path+"/upload")

	// if using the welcome server, just return OK without deleting anything
	if (!no_upload) {

		// if err != nil {
		// 	debug(2, "File not found: %s", err)
		// 	http.NotFound(writer, request)
		// 	service.debug_info.requestServed(int64(0))
		// 	log("\"POST %s\" 404 0 \"%s\"", query, ua)
		// 	return
		// }

		// max size is 20MB of memory
		err := request.ParseMultipartForm(32 << 20)

		if err != nil {
			debug(2, "Error parsing imag: %s", err.Error())
			writer.WriteHeader(http.StatusPreconditionFailed)
			service.debug_info.requestServed(int64(0))
			log("\"POST %s\" 412 0 \"%s\"", query, ua)
			return
		}

		// debug(2, "Form data: %s", values)
		file, handler, err := request.FormFile("file")
		if err != nil {
			debug(2, "Error finding uploaded file: %s", err.Error())
			writer.WriteHeader(http.StatusExpectationFailed)
			service.debug_info.requestServed(int64(0))
			log("\"POST %s\" 417 0 \"%s\"", query, ua)
			return
		}
		defer file.Close()

		// FIXME -- check the filename so it does not start with dots, or slashes!
		full_path, _ := service.fullPathToFile(share, path+"/"+handler.Filename)

		f, err := os.OpenFile(full_path, os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			debug(2, "Error creating uploaded file: %s", err.Error())
			writer.WriteHeader(http.StatusServiceUnavailable)
			service.debug_info.requestServed(int64(0))
			log("\"POST %s\" 503 0 \"%s\"", query, ua)
			return
		}
		defer f.Close()
		io.Copy(f, file)

		debug(2, "POST of a file upload parsed successfully")

	}	else {
		debug(2, "NOTICE: Running in no-upload mode.")
	}

	writer.WriteHeader(http.StatusOK)

	return
}
