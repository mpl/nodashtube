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

// modtime is the modification time of the resource to be served, or IsZero().
// return value is whether this request is now complete.
func checkLastModified(w http.ResponseWriter, r *http.Request, modtime time.Time) bool {
	if modtime.IsZero() {
		return false
	}

	// The Date-Modified header truncates sub-second precision, so
	// use mtime < t+1s instead of mtime <= t to check for unmodified.
	if t, err := time.Parse(http.TimeFormat, r.Header.Get("If-Modified-Since")); err == nil && modtime.Before(t.Add(1*time.Second)) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	w.Header().Set("Last-Modified", modtime.UTC().Format(http.TimeFormat))
	return false
}

// name is '/'-separated, not filepath.Separator.
func serveFile(w http.ResponseWriter, r *http.Request, fs http.FileSystem, name string) {
	const indexPage = "/index.html"

	f, err := fs.Open(name)
	if err != nil {
		// TODO expose actual error?
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	d, err1 := f.Stat()
	if err1 != nil {
		// TODO expose actual error?
		http.NotFound(w, r)
		return
	}

	// use contents of index.html for directory, if present
	if d.IsDir() {
		index := name + indexPage
		ff, err := fs.Open(index)
		if err == nil {
			defer ff.Close()
			dd, err := ff.Stat()
			if err == nil {
				name = index
				d = dd
				f = ff
			}
		}
	}

	// Still a directory? (we didn't find an index.html file)
	if d.IsDir() {
		if checkLastModified(w, r, d.ModTime()) {
			return
		}
		sortedDirList(w, f)
		return
	}

	// serverContent will check modification time
	http.ServeContent(w, r, d.Name(), d.ModTime(), f)
}

func myFileServer(w http.ResponseWriter, r *http.Request, url string) {
	//	http.ServeFile(w, r, path.Join(rootdir, url))
	dir, file := filepath.Split(filepath.Join("ROOTDIR", url))
	serveFile(w, r, http.Dir(dir), file)
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
	tpl           = template.Must(template.New("main").Parse(indexHTML))
	tempDir       = func() string {
		if *dlDir == "" {
			return filepath.Join(os.TempDir(), "nodashtube")
		}
		return *dlDir
	}()

	inProgressMu sync.RWMutex
	inProgress   = make(map[string]*dlInfo)
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

	http.HandleFunc("/", mainHandler)
	http.HandleFunc("/youtube", youtubeHandler)
	http.HandleFunc("/kill", killHandler)
	if err := http.ListenAndServe(*host, nil); err != nil {
		log.Fatalf("Could not start http server: %v", err)
	}
}

func mainHandler(w http.ResponseWriter, r *http.Request) {
	inProgressMu.RLock()
	defer inProgressMu.RUnlock()
	dat := struct{ InProgress map[string]*dlInfo }{InProgress: inProgress}
	if err := tpl.Execute(w, &dat); err != nil {
		log.Printf("Could not execute template: %v", err)
	}
}

// TODO(mpl): make it a stringer and remove progress out of it?
type dlInfo struct {
	URL      string
	progress string      // get it with a chan from the proc output. or something.
	proc     *os.Process // so we can kill it
}

func youtubeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Want POST", http.StatusMethodNotAllowed)
		return
	}
	youtubeURL := r.PostFormValue(urlInputName)
	if youtubeURL == "" {
		return
	}
	println(youtubeURL)
	inProgressMu.Lock()
	defer inProgressMu.Unlock()
	if _, ok := inProgress[youtubeURL]; ok {
		log.Printf("Not starting %v because it is already in progress", youtubeURL)
		return
	}

	cmd := exec.Command("youtube-dl", youtubeURL)
	cmd.Dir = tempDir
	if err := cmd.Start(); err != nil {
		log.Printf("Could not start youtube-dl with %v: %v", youtubeURL, err)
		// TODO(mpl): refresh. everywhere.
		return
	}
	log.Printf("Starting download of %v", youtubeURL)
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("youtube-dl %v didn't finish successfully: %v", youtubeURL, err)
		}
	}()
	inProgress[youtubeURL] = &dlInfo{
		URL:      youtubeURL,
		progress: "Started",
		proc:     cmd.Process,
	}
	dat := struct{ InProgress map[string]*dlInfo }{InProgress: inProgress}
	if err := tpl.Execute(w, &dat); err != nil {
		log.Printf("Could not execute template: %v", err)
	}
}

func killHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Want POST", http.StatusMethodNotAllowed)
		return
	}
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
	dat := struct{ InProgress map[string]*dlInfo }{InProgress: inProgress}
	if err := tpl.Execute(w, &dat); err != nil {
		log.Printf("Could not execute template: %v", err)
	}
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
