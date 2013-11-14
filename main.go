package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
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
	host   = flag.String("host", "0.0.0.0:8080", "listening port and hostname.")
	prefix = flag.String("prefix", "", "URL prefix for which the server runs (as in http://foo:8080/prefix).")
)

func usage() {
	fmt.Fprintf(os.Stderr, "\t nodashtube \n")
	flag.PrintDefaults()
	os.Exit(2)
}

var (
	urlInputName  = "youtubeURL"
	killInputName = "toKill"
	partialParam  = "partialFile"

	prefixes = map[string]string{
		"main":     "/",
		"youtube":  "/youtube",
		"kill":     "/kill",
		"stored":   "/stored/",
		"progress": "/progress",
		"notify":   "/notify.js",
		"partial":  "/partial",
	}

	tempDir string

	inProgressMu sync.RWMutex
	inProgress   = make(map[string]*dlInfo)

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

	// these have to be redefined now because of *prefix flag
	// that is set after glob vars have been initialized.
	for k, v := range prefixes {
		prefixes[k] = path.Join(*prefix, v)
	}
	// TODO(mpl): sucks
	prefixes["stored"] = prefixes["stored"] + "/"

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
	http.HandleFunc(prefixes["progress"], listHandler)
	/*
		http.HandleFunc(prefixes["notify"], func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, "/home/mpl/gocode/src/github.com/mpl/nodashtube/notify.js")
		})
	*/
	http.HandleFunc(prefixes["partial"], partialHandler)
	http.HandleFunc(prefixes["main"], mainHandler)
	if err := http.ListenAndServe(*host, nil); err != nil {
		log.Fatalf("Could not start http server: %v", err)
	}
}

func mainHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != prefixes["main"] {
		println(r.URL.Path + " != " + prefixes["main"])
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
			log.Printf("youtube-dl %v didn't finish successfully: %v", youtubeURL, err)
			return
		}
		inProgressMu.Lock()
		delete(inProgress, youtubeURL)
		inProgressMu.Unlock()
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
		return
	}
	delete(inProgress, toKill)
}

func storedHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Want GET", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, prefixes["stored"])
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
<!--		<script src='notify.js'></script> -->
	</head>

	<body>
	<script>
var currentList = [];
setInterval(function(){getProgressList("` + prefixes["progress"] + `")},5000);

function notify(URL) {
	if (!(window.webkitNotifications)) {
		console.log("Notifications not supported");
		return;
	}
	// TODO(mpl): probably should be tried once first on load or something?
	var havePermission = window.webkitNotifications.checkPermission();
	if (havePermission != 0) {
		window.webkitNotifications.requestPermission();
		return;
	}
	// 0 is PERMISSION_ALLOWED
	// TODO(mpl): video title in text
	// TODO(mpl): try without the icon
//		'http://i.stack.imgur.com/dmHl0.png',
	var notification = window.webkitNotifications.createNotification(
		'',
		'NoDashTube notification',
		URL + ' is done.'
	);

	// TODO(mpl): open the stored vid
	notification.onclick = function () {
		window.open("http://` + path.Join(*host, *prefix) + `");
		notification.close();
	}
	notification.show();
} 

function getProgressList(URL) {
	var xmlhttp = new XMLHttpRequest();
	xmlhttp.open("GET",URL,false);
	xmlhttp.send();
	console.log(xmlhttp.responseText);
// TODO(mpl): better error handling.
	var newListJSON = xmlhttp.response;
	var newList = Object.keys(JSON.parse(newListJSON));
	console.log(newList.length);
	console.log(newList);
	if (currentList.length == 0) {
		currentList = newList;
		return;
	}
	for (var i=0; i<currentList.length; i++) {
		var youtubeURL = currentList[i];
		var found = 0;
		for (var j=0; j<newList.length; j++) {
			if (youtubeURL === newList[j]) {
				found = 1;
				break;
			}
		}
		if (found == 0) {
			console.log(youtubeURL + " is done.")
			notify(youtubeURL);
		}
	}
	currentList = newList;
}
	</script>

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
<!--
		<td>
			<form action="` + prefixes["partial"] + `" method="GET" id="partialform">
			<input type="hidden" id="partialurl" name="` + partialParam + `" value="{{$dl.URL}}">
			<input type="submit" id="partialsubmit" value="Play">
			</form>
		</td>
-->
	</tr>
	{{end}}
	</table>
	{{end}}
	{{if .Stored}}
	<h2> Stored </h2>
	<table>
	{{range $st := .Stored}}
	<tr>
		<td><a href="` + prefixes["stored"] + `{{$st}}">{{$st}}</a></td>
	</tr>
	{{end}}
	</table>
	{{end}}
	</body>
</html>
`
}
