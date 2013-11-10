package main

import (
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	idstring = "http://golang.org/pkg/http/#ListenAndServe"
)

var (
	host  = flag.String("host", "0.0.0.0:8080", "listening port and hostname")
	dlDir = flag.String("downloaddir", "", "where to write the downloads. defaults to /tmp/nodashtube.")
	help  = flag.Bool("h", false, "show this help")
)

type sortedFiles []os.FileInfo

func (s sortedFiles) Len() int { return len(s) }

func (s sortedFiles) Less(i, j int) bool {
	return strings.ToLower(s[i].Name()) < strings.ToLower(s[j].Name())
}

func (s sortedFiles) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func sortedDirList(w http.ResponseWriter, f http.File) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<pre>\n")
	var sdirs sortedFiles
	for {
		dirs, err := f.Readdir(100)
		if err != nil || len(dirs) == 0 {
			break
		}
		sdirs = append(sdirs, dirs...)
	}
	sort.Sort(sdirs)
	for _, d := range sdirs {
		name := d.Name()
		if d.IsDir() {
			name += "/"
		}
		// TODO htmlescape
		fmt.Fprintf(w, "<a href=\"%s\">%s</a>\n", name, name)
	}
	fmt.Fprintf(w, "</pre>\n")
}

func usage() {
	fmt.Fprintf(os.Stderr, "\t nodashtube \n")
	flag.PrintDefaults()
	os.Exit(2)
}

var (
	urlInputName  = "youtubeURL"
	killInputName = "toKill"

	youtubePrefix = "/youtube"
	killPrefix    = "/kill"
	storedPrefix  = "/stored/"

	tempDir = func() string {
		if *dlDir == "" {
			return filepath.Join(os.TempDir(), "nodashtube")
		}
		return *dlDir
	}()

	inProgressMu sync.RWMutex
	inProgress   = make(map[string]*dlInfo)

	storedMu sync.RWMutex
	stored   []string

	lastMod time.Time

	tpl = template.Must(template.New("main").Parse(indexHTML))
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

	if err := os.MkdirAll(tempDir, 0755); err != nil {
		log.Fatalf("Could not create temp dir %v: %v", tempDir, err)
	}
	refreshStored(time.Now())

	http.HandleFunc(youtubePrefix, youtubeHandler)
	http.HandleFunc(killPrefix, killHandler)
	http.HandleFunc(storedPrefix, storedHandler)
	http.HandleFunc("/", mainHandler)
	if err := http.ListenAndServe(*host, nil); err != nil {
		log.Fatalf("Could not start http server: %v", err)
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

	// The Date-Modified header truncates sub-second precision, so
	// use mtime < t+1s instead of mtime <= t to check for unmodified.
	if d.ModTime().Before(since.Add(1 * time.Second)) {
		storedMu.Lock()
		defer storedMu.Unlock()
		stored, err = sortedStored(f)
		if err != nil {
			log.Fatal(err)
		}
		if lastMod.Before(d.ModTime()) {
			lastMod = d.ModTime()
		}
		return true
	}
	return false
}

// TODO(mpl): move in above
func sortedStored(f *os.File) ([]string, error) {
	// TODO(mpl): filter with .part?
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func refresh(w http.ResponseWriter, r *http.Request) {
	ifMod, err := time.Parse(http.TimeFormat, r.Header.Get("If-Modified-Since"))
	if err != nil {
		ifMod = time.Now()
	}
	refreshed := refreshStored(ifMod)
	if !refreshed && lastMod.Before(ifMod) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Last-Modified", lastMod.UTC().Format(http.TimeFormat))
	storedMu.RLock()
	dat := struct {
		InProgress map[string]*dlInfo
		Stored     []string
	}{
		InProgress: inProgress,
		Stored:     stored,
	}
	storedMu.RUnlock()
	if err := tpl.Execute(w, &dat); err != nil {
		log.Printf("Could not execute template: %v", err)
	}
}

func mainHandler(w http.ResponseWriter, r *http.Request) {
	inProgressMu.RLock()
	defer inProgressMu.RUnlock()
	refresh(w, r)
}

// TODO(mpl): make it a stringer and remove progress out of it?
type dlInfo struct {
	URL      string
	progress string      // get it with a chan from the proc output. or something.
	proc     *os.Process // so we can kill it
}

func youtubeHandler(w http.ResponseWriter, r *http.Request) {
	// TODO(mpl): reset urlpath
	if r.Method != "POST" {
		http.Error(w, "Want POST", http.StatusMethodNotAllowed)
		mainHandler(w, r)
		return
	}
	youtubeURL := r.PostFormValue(urlInputName)
	if youtubeURL == "" {
		mainHandler(w, r)
		return
	}
	println(youtubeURL)
	inProgressMu.Lock()
	defer inProgressMu.Unlock()
	defer refresh(w, r)
	if _, ok := inProgress[youtubeURL]; ok {
		log.Printf("Not starting %v because it is already in progress", youtubeURL)
		return
	}

	//	cmd := exec.Command("youtube-dl", youtubeURL)
	cmd := exec.Command("wget", youtubeURL)
	cmd.Dir = tempDir
	if err := cmd.Start(); err != nil {
		log.Printf("Could not start youtube-dl with %v: %v", youtubeURL, err)
		return
	}
	log.Printf("Starting download of %v", youtubeURL)
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("youtube-dl %v didn't finish successfully: %v", youtubeURL, err)
		}
		inProgressMu.Lock()
		delete(inProgress, youtubeURL)
		inProgressMu.Unlock()
		lastMod = time.Now()
	}()
	inProgress[youtubeURL] = &dlInfo{
		URL:      youtubeURL,
		progress: "Started",
		proc:     cmd.Process,
	}
}

func killHandler(w http.ResponseWriter, r *http.Request) {
	// TODO(mpl): reset urlpath
	if r.Method != "POST" {
		http.Error(w, "Want POST", http.StatusMethodNotAllowed)
		mainHandler(w, r)
		return
	}
	toKill := r.PostFormValue(killInputName)
	if toKill == "" {
		mainHandler(w, r)
		return
	}
	inProgressMu.Lock()
	defer inProgressMu.Unlock()
	defer refresh(w, r)
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

func isStored(name string) bool {
	for _, v := range stored {
		if v == name {
			return true
		}
	}
	return false
}

func storedHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Want GET", http.StatusMethodNotAllowed)
		mainHandler(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, storedPrefix)
	println("WANT " + name)
	storedMu.RLock()
	defer storedMu.RUnlock()
	if !isStored(name) {
		http.NotFound(w, r)
	}
	http.ServeFile(w, r, filepath.Join(tempDir, name))
}

var indexHTML = `
<!DOCTYPE HTML>
<html>
	<head>
		<title>NoDashTube</title>
	</head>

	<body>
	<form action="` + youtubePrefix + `" method="POST" id="youtubeform">
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
			<form action="` + killPrefix + `" method="POST" id="killform">
			<input type="hidden" id="killurl" name="` + killInputName + `" value="{{$dl.URL}}">
			<input type="submit" id="killsubmit" value="Cancel">
			</form>
		</td>
	</tr>
	{{end}}
	</table>
	{{end}}
	{{if .Stored}}
	<h2> Stored </h2>
	<table>
	{{range $st := .Stored}}
	<tr>
		<td><a href="` + storedPrefix + `{{$st}}">{{$st}}</a></td>
	</tr>
	{{end}}
	</table>
	{{end}}
	</body>
</html>
`

/*
func makeHandler(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if e, ok := recover().(error); ok {
				http.Error(w, e.Error(), http.StatusInternalServerError)
				return
			}
		}()
		title := r.URL.Path
		w.Header().Set("Server", idstring)
		if isAllowed(r) {
			fn(w, r, title)
		} else {
			sendUnauthorized(w, r)
		}
	}
}
*/
