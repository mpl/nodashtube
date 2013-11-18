package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	idstring = "http://golang.org/pkg/http/#ListenAndServe"
)

var (
	dlDir  = flag.String("dldir", "", "where to write the downloads. defaults to /tmp/nodashtube.")
	help   = flag.Bool("h", false, "show this help.")
	host   = flag.String("host", "localhost:8080", "listening port and hostname.")
	prefix = flag.String("prefix", "", "URL prefix for which the server runs (as in http://foo:8080/prefix).")
)

func usage() {
	fmt.Fprintf(os.Stderr, "nodashtube \n")
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "%v\n", examples())
	os.Exit(2)
}

func examples() string {
	return `
Examples:
	nodashtube -dldir $HOME/Downloads/nodashtube -prefix nodashtube
`
}

var (
	urlInputName  = "youtubeURL"
	killInputName = "toKill"
	partialParam  = "partialFile"

	prefixes = map[string]string{
		"main":    "/",
		"youtube": "/youtube",
		"kill":    "/kill",
		"stored":  "/stored/",
		"list":    "/list",
		"partial": "/partial",
		"done":    "/done",
	}

	tempDir string

	inProgressMu sync.RWMutex
	inProgress   = make(map[string]*dlInfo)

	doneMu sync.RWMutex
	done   = make(map[string]string) // map URL to filename, for javascript.

	storedMu sync.RWMutex
	stored   []string
	lastMod  time.Time

	tpl *template.Template
)

func main() {
	flag.Usage = usage
	flag.Parse()
	if *help {
		usage()
	}

	nargs := flag.NArg()
	if nargs > 0 {
		usage()
	}

	if *prefix != "" && (*prefix)[0] != '/' {
		*prefix = "/" + *prefix
	}
	// these have to be redefined now because of *prefix flag
	// that is set after glob vars have been initialized.
	for k, v := range prefixes {
		trailingSlash := false
		if prefixes[k][len(prefixes[k])-1] == '/' {
			trailingSlash = true
		}
		prefixes[k] = path.Join(*prefix, v)
		if trailingSlash {
			prefixes[k] = prefixes[k] + "/"
		}
	}

	tpl = template.Must(template.New("main").Parse(mainHTML()))
	tempDir = func() string {
		if *dlDir == "" {
			return filepath.Join(os.TempDir(), "nodashtube")
		}
		return *dlDir
	}()

	if err := os.MkdirAll(tempDir, 0755); err != nil {
		log.Fatalf("Could not create temp dir %v: %v", tempDir, err)
	}
	refreshStored(time.Time{})

	// TODO(mpl): favicon.ico
	http.HandleFunc(prefixes["youtube"], youtubeHandler)
	http.HandleFunc(prefixes["kill"], killHandler)
	http.HandleFunc(prefixes["stored"], storedHandler)
	http.HandleFunc(prefixes["done"], doneHandler)
	http.HandleFunc(prefixes["list"], listHandler)
	http.HandleFunc(prefixes["partial"], partialHandler)
	http.HandleFunc(prefixes["main"], mainHandler)
	if err := http.ListenAndServe(*host, nil); err != nil {
		log.Fatalf("Could not start http server: %v", err)
	}
}

func mainHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != prefixes["main"] {
		http.NotFound(w, r)
		return
	}
	refresh(w, r)
}

func youtubeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Want POST", http.StatusMethodNotAllowed)
		return
	}
	// TODO(mpl): is there a lighter way to do that? I just want
	// the url bar path to be changed to "/".
	defer http.Redirect(w, r, prefixes["main"], http.StatusSeeOther)
	youtubeURL := r.PostFormValue(urlInputName)
	if youtubeURL == "" {
		mainHandler(w, r)
		return
	}
	inProgressMu.Lock()
	defer inProgressMu.Unlock()
	if _, ok := inProgress[youtubeURL]; ok {
		log.Printf("Not starting %v because it is already in progress", youtubeURL)
		return
	}

	// TODO(mpl): help with installing youtube-dl
	cmd := exec.Command("youtube-dl", youtubeURL)
	out := progressWriter{}
	cmd.Stdout = &out
	//	cmd := exec.Command("wget", youtubeURL)
	cmd.Dir = tempDir
	if err := cmd.Start(); err != nil {
		log.Printf("Could not start youtube-dl with %v: %v", youtubeURL, err)
		return
	}
	log.Printf("Starting download of %v", youtubeURL)
	go func() {
		if err := cmd.Wait(); err != nil {
			// TODO(mpl): capture and print youtube-dl's process stderr here
			log.Printf("youtube-dl %v didn't finish successfully: %v", youtubeURL, err)
			inProgressMu.Lock()
			defer inProgressMu.Unlock()
			delete(inProgress, youtubeURL)
			return
		}
		inProgressMu.Lock()
		defer inProgressMu.Unlock()
		storedMu.Lock()
		defer storedMu.Unlock()
		refreshStored(time.Time{})
		// TODO(mpl): prune done after some time or some event?
		// Like maybe after stored[filename] has been hit at least once?
		doneMu.Lock()
		defer doneMu.Unlock()
		done[youtubeURL] = inProgress[youtubeURL].Filename
		delete(inProgress, youtubeURL)
		log.Printf("%v done.", youtubeURL)
	}()
	info := &dlInfo{
		URL:      youtubeURL,
		Progress: &out,
		proc:     cmd.Process,
	}
	out.parent = info
	info.Progress = &out
	inProgress[youtubeURL] = info
}

func killHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Want POST", http.StatusMethodNotAllowed)
		return
	}
	defer http.Redirect(w, r, prefixes["main"], http.StatusSeeOther)
	toKill := r.PostFormValue(killInputName)
	if toKill == "" {
		return
	}
	inProgressMu.Lock()
	defer inProgressMu.Unlock()
	dl, ok := inProgress[toKill]
	if !ok {
		log.Printf("Could not cancel %v, because not in progress.", toKill)
		return
	}
	if err := dl.proc.Kill(); err != nil {
		log.Printf("Could not kill %v: %v", toKill, err)
	}
	delete(inProgress, toKill)
}

func storedHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Want GET", http.StatusMethodNotAllowed)
		return
	}
	name, err := url.QueryUnescape(strings.TrimPrefix(r.URL.Path, prefixes["stored"]))
	if err != nil {
		http.Error(w, "Error unescaping requested filename", http.StatusInternalServerError)
		return
	}
	storedMu.RLock()
	defer storedMu.RUnlock()
	if !isStored(name) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(tempDir, name))
}

func listHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Want GET", http.StatusMethodNotAllowed)
		return
	}
	inProgressMu.RLock()
	defer inProgressMu.RUnlock()
	progressJSON, err := json.Marshal(inProgress)
	if err != nil {
		// TODO(mpl): json error
		http.Error(w, "Error encoding progress", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/javascript")
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Length", strconv.Itoa(len(progressJSON)+1))
	w.Write(progressJSON)
	w.Write([]byte("\n"))
}

func doneHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Want GET", http.StatusMethodNotAllowed)
		return
	}

	url := r.FormValue("url")
	if url == "" {
		http.Error(w, fmt.Sprintf("request has no \"url\" param"), http.StatusBadRequest)
		return
	}
	doneMu.RLock()
	defer doneMu.RUnlock()
	filename, ok := done[url]
	if !ok {
		http.NotFound(w, r)
		return
	}
	doneJSON, err := json.Marshal(struct{ Filename string }{Filename: filename})
	if err != nil {
		// TODO(mpl): json error
		http.Error(w, "Error encoding done", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/javascript")
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Length", strconv.Itoa(len(doneJSON)+1))
	w.Write(doneJSON)
	w.Write([]byte("\n"))
}

func partialHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Want GET", http.StatusMethodNotAllowed)
		return
	}
	url := r.FormValue(partialParam)
	if url == "" {
		http.Error(w, fmt.Sprintf("request has no %v param", partialParam), http.StatusBadRequest)
		return
	}
	inProgressMu.RLock()
	defer inProgressMu.RUnlock()
	info, ok := inProgress[url]
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(tempDir, info.Filename+".part"))
}

func refresh(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", idstring)
	ifMod, err := time.Parse(http.TimeFormat, r.Header.Get("If-Modified-Since"))
	if err != nil {
		ifMod = time.Time{}
	}

	storedMu.Lock()
	defer storedMu.Unlock()
	refreshed := refreshStored(ifMod)
	inProgressMu.RLock()
	defer inProgressMu.RUnlock()
	if len(inProgress) == 0 && !refreshed {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Last-Modified", lastMod.UTC().Format(http.TimeFormat))
	dat := struct {
		InProgress map[string]*dlInfo
		Stored     []string
	}{
		InProgress: inProgress,
		Stored:     stored,
	}
	if err := tpl.Execute(w, &dat); err != nil {
		log.Printf("Could not execute template: %v", err)
	}
}

func refreshStored(since time.Time) bool {
	f, err := os.Open(tempDir)
	if err != nil {
		log.Fatalf("Could not open temp dir %v: %v", tempDir, err)
	}
	defer f.Close()

	d, err := f.Stat()
	if err != nil {
		log.Fatalf("Could not stat temp dir %v: %v", tempDir, err)
	}

	if !d.IsDir() {
		log.Fatalf("%v not a dir", tempDir)
	}

	if d.ModTime().After(since) {
		stored, err = sortedStored(f)
		if err != nil {
			log.Fatal(err)
		}
		lastMod = d.ModTime()
		return true
	}
	return false
}

func sortedStored(f *os.File) ([]string, error) {
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	onlyFull := []string{}
	for _, v := range names {
		if !strings.HasSuffix(v, ".part") {
			onlyFull = append(onlyFull, v)
		}
	}
	sort.Strings(onlyFull)
	return onlyFull, nil
}

type dlInfo struct {
	URL      string
	Filename string
	Progress *progressWriter
	proc     *os.Process // so we can kill it
}

type progressWriter struct {
	sync.RWMutex // only locks lastLine
	lastLine     string

	// no lock needed on these, as long as we don't do concurrent writes
	buf          bytes.Buffer
	filenameDone bool

	parent *dlInfo // needs to be locked with inProgressMu
}

const destPattern = "[download] Destination:"

func (prw *progressWriter) Write(p []byte) (n int, err error) {
	n, err = prw.buf.Write(p)
	if err != nil {
		return n, err
	}

	contents := prw.buf.String()
	if len(contents) > 0 {
		if !prw.filenameDone {
			sidx := strings.Index(contents, destPattern)
			if sidx != -1 && len(contents) > len(destPattern)+1 {
				sidx = sidx + len(destPattern) + 1
				if eidx := strings.Index(contents[sidx:], "\n"); eidx != -1 {
					inProgressMu.Lock()
					prw.parent.Filename = contents[sidx : sidx+eidx]
					inProgressMu.Unlock()
					prw.filenameDone = true
				}
			}
			return n, err
		}
		cleanEnd := strings.LastIndex(contents, "\r")
		if cleanEnd == -1 {
			return n, err
		}
		cleanStart := strings.LastIndex(contents[:cleanEnd], "\n")
		if cleanStart == -1 {
			cleanStart = 0
		}
		prw.Lock()
		prw.lastLine = contents[cleanStart:cleanEnd]
		prw.Unlock()
		prw.buf.Read(make([]byte, cleanEnd+1))
	}
	return n, err
}

func (prw *progressWriter) String() string {
	prw.RLock()
	defer prw.RUnlock()
	return prw.lastLine
}

func isStored(name string) bool {
	for _, v := range stored {
		if v == name {
			return true
		}
	}
	return false
}

func mainHTML() string {
	return `<!DOCTYPE HTML>
<html>
	<head>
		<title>NoDashTube</title>
	</head>

	<body>
	<script>
var oldList = {};
setInterval(function(){getDownloadsList("` + prefixes["list"] + `")},7000);
window.onload=function(){getDownloadsList("` + prefixes["list"] + `")};

function enableNotify() {
	if (!(window.webkitNotifications)) {
		alert("Notifications not supported on this browser.");
		return;
	}
	var havePermission = window.webkitNotifications.checkPermission();
	if (havePermission == 0) {
		alert("Notifications already allowed.");
		return;
	}
	window.webkitNotifications.requestPermission();
}

function notify(filename) {
	if (!(window.webkitNotifications)) {
		console.log("Notifications not supported");
		return;
	}
	var havePermission = window.webkitNotifications.checkPermission();
	if (havePermission != 0) {
		console.log("Notifications not allowed.");
		return;
	}
	var notification = window.webkitNotifications.createNotification(
		'',
		'NoDashTube notification',
		filename + ' is done.'
	);

	notification.onclick = function () {
		window.open("http://` + *host + prefixes["stored"] + `" + encodeURIComponent(filename));
		notification.close();
	}
	notification.show();
} 

function getFilename(url) {
	var xmlhttp = new XMLHttpRequest();
	xmlhttp.open("GET",url,false);
	xmlhttp.send();
	console.log(xmlhttp.responseText);
// TODO(mpl): better error handling.
	var filenameJSON = xmlhttp.response;
	var filenameObj = JSON.parse(filenameJSON);
	if (!(filenameObj.Filename)) {
		return "";
	}
	return filenameObj.Filename;
}

function getDownloadsList(url) {
	var xmlhttp = new XMLHttpRequest();
	xmlhttp.open("GET",url,false);
	xmlhttp.send();
	console.log(xmlhttp.responseText);
// TODO(mpl): better error handling.
	var newListJSON = xmlhttp.response;
	var newList = JSON.parse(newListJSON);
	var newKeys = Object.keys(newList);
	var oldKeys = Object.keys(oldList);
	console.log(newKeys);
	if (oldKeys.length == 0) {
		oldList = newList;
		return;
	}
	for (var i=0; i<oldKeys.length; i++) {
		var found = 0;
		for (var j=0; j<newKeys.length; j++) {
			if (oldKeys[i] === newKeys[j]) {
				found = 1;
				break;
			}
		}
		if (found == 0) {
			console.log(oldKeys[i] + " is done.");
			var newlyStored = getFilename("` + prefixes["done"] + `?url=" + oldKeys[i]);
			if (newlyStored == "") {
				return;
			}
			console.log(newlyStored + " is done.")
			notify(newlyStored);
		}
	}
	oldList = newList;
}
	</script>

	<a id="notifyLink" href="#" onclick="enableNotify();return false;">Enable notifications?</a>

	<h2> Enter a youtube URL </h2>
	<form action="` + prefixes["youtube"] + `" method="POST" id="youtubeform">
	<input type="url" id="youtubeurl" name="` + urlInputName + `">
	<input type="submit" id="urlsubmit" value="Download">
	</form>
	{{if .InProgress}}
	<h2> In progress </h2>
	<table>
	{{range $dl := .InProgress}}
	<tr>
		<td>{{$dl.URL}}</td>
		<td>
			<form action="` + prefixes["kill"] + `" method="POST" id="killform">
			<input type="hidden" id="killurl" name="` + killInputName + `" value="{{$dl.URL}}">
			<input type="submit" id="killsubmit" value="Cancel">
			</form>
		</td>
	</tr>
	<tr>
<!-- TODO(mpl): filename in the response? -->
		<td><a href="` + prefixes["partial"] + `?` + partialParam + `={{urlquery $dl.URL}}">
			{{if $dl.Filename}}{{$dl.Filename}}{{else}}{{$dl.URL}}{{end}}.part</a></td>
	</tr>
	<tr>
		<td><pre>{{$dl.Progress}}</pre></td>
	</tr>
	{{end}}
	</table>
	{{end}}
	{{if .Stored}}
	<h2> Stored </h2>
	<table>
	{{range $st := .Stored}}
	<tr>
		<td><a href="` + prefixes["stored"] + `{{urlquery $st}}">{{$st}}</a></td>
	</tr>
	{{end}}
	</table>
	{{end}}
	</body>
</html>
`
}
