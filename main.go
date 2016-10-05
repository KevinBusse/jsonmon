/*
Quick and simple monitoring system

Usage:
 jsonmon config.yml
 jsonmon -v # Prints version to stdout and exits

Environment:
 HOST: HTTP network interface, defaults to localhost
 PORT: HTTP port, defaults to 3000

More docs: https://github.com/chillum/jsonmon/wiki
*/
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v2"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Version is the application version.
const Version = "3.2-alpha"

// This one is for internal use.
type ver struct {
	App  string `json:"jsonmon"`
	Go   string `json:"runtime"`
	Os   string `json:"os"`
	Arch string `json:"arch"`
}

var version ver

// Check details.
type Check struct {
	Name   string `json:"name,omitempty" yaml:"name"`
	Web    string `json:"web,omitempty" yaml:"web"`
	Shell  string `json:"shell,omitempty" yaml:"shell"`
	Match  string `json:"-" yaml:"match"`
	Return int    `json:"-" yaml:"return"`
	Notify string `json:"-" yaml:"notify"`
	Alert  string `json:"-" yaml:"alert"`
	Tries  int    `json:"-" yaml:"tries"`
	Repeat int    `json:"-" yaml:"repeat"`
	Sleep  int    `json:"-" yaml:"sleep"`
	Failed bool   `json:"failed" yaml:"-"`
	Since  string `json:"since,omitempty" yaml:"-"`
}

// Global checks list. Need to share it with workers and Web UI.
var checks []Check

// Global started and last modified date for HTTP caching.
var modified string
var started string
var modHTML string
var modAngular string
var modJS string
var modCSS string

var mutex *sync.RWMutex

// Construct the last modified string.
func etag(ts time.Time) string {
	return "W/\"" + strconv.FormatInt(ts.UnixNano(), 10) + "\""
}

// The main loop.
func main() {
	// Parse CLI args.
	usage := "Usage: " + path.Base(os.Args[0]) + " config.yml\n" +
		"Docs:  https://github.com/chillum/jsonmon/wiki"
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	// -v for version.
	version.App = Version
	version.Go = runtime.Version()
	version.Os = runtime.GOOS
	version.Arch = runtime.GOARCH
	switch os.Args[1] {
	case "-h":
		fallthrough
	case "-help":
		fallthrough
	case "--help":
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(0)
	case "-v":
		fallthrough
	case "-version":
		fallthrough
	case "--version":
		json, _ := json.Marshal(&version)
		fmt.Println(string(json))
		os.Exit(0)
	}
	// Read config file or exit with error.
	config, err := ioutil.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprint(os.Stderr, "<2>", err, "\n")
		os.Exit(3)
	}
	err = yaml.Unmarshal(config, &checks)
	if err != nil {
		fmt.Fprint(os.Stderr, "<2>", "invalid config at ", os.Args[1], "\n", err, "\n")
		os.Exit(3)
	}
	// Exit with return code 0 on kill.
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGTERM)
	go func() {
		<-done
		os.Exit(0)
	}()
	// Run checks and init HTTP cache.
	started = etag(time.Now())
	modified = started
	mutex = &sync.RWMutex{}
	for i := range checks {
		go worker(&checks[i])
	}
	cacheHTML, _ := AssetInfo("index.html"); modHTML = cacheHTML.ModTime().UTC().Format(http.TimeFormat)
	cacheAngular, _ := AssetInfo("angular.min.js"); modAngular = cacheAngular.ModTime().UTC().Format(http.TimeFormat)
	cacheJS, _ := AssetInfo("app.js"); modJS = cacheJS.ModTime().UTC().Format(http.TimeFormat)
	cacheCSS, _ := AssetInfo("main.css"); modCSS = cacheCSS.ModTime().UTC().Format(http.TimeFormat)
	// Launch the Web server.
	host := os.Getenv("HOST")
	port := os.Getenv("PORT")
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "3000"
	}
	http.HandleFunc("/status", getChecks)
	http.HandleFunc("/version", getVersion)
	http.HandleFunc("/", getUI)
	fmt.Fprint(os.Stderr, "<7>Starting HTTP service at ", host, ":", port, "\n")
	err = http.ListenAndServe(host+":"+port, nil)
	if err != nil {
		fmt.Fprint(os.Stderr, "<2>", err, "\n")
		fmt.Fprintln(os.Stderr, "<7>Use HOST and PORT env variables to customize server settings")
	}
	os.Exit(4)
}

// Background worker.
func worker(check *Check) {
	if check.Shell == "" && check.Web == "" {
		fmt.Fprintln(os.Stderr, "<4>Ignoring entry with no either Web or shell check")
		mutex.Lock()
		check.Failed = true
		mutex.Unlock()
		return
	}
	if check.Shell != "" && check.Web != "" {
		fmt.Fprint(os.Stderr,
			"<3>Web and shell checks in one block are not allowed\n",
			"<3>Disabled: ", check.Shell, "\n",
			"<3>Disabled: ", check.Web, "\n")
		mutex.Lock()
		check.Failed = true
		mutex.Unlock()
		return
	}
	mutex.Lock()
	if check.Repeat == 0 { // Set default timeout.
		check.Repeat = 30
	}
	if check.Tries == 0 { // Default to 1 attempt.
		check.Tries = 1
	}
	mutex.Unlock()
	repeat := time.Second * time.Duration(check.Repeat)
	sleep := time.Second * time.Duration(check.Sleep)
	var name string
	if check.Web != "" {
		if check.Name != "" { // Set check's display name.
			name = check.Name
		} else {
			name = check.Web
		}
		if check.Return == 0 { // Successful HTTP return code is 200.
			mutex.Lock()
			check.Return = 200
			mutex.Unlock()
		}
		for {
			web(check, &name, &sleep)
			time.Sleep(repeat)
		}
	} else {
		if check.Name != "" { // Set check's display name.
			name = check.Name
		} else {
			name = check.Shell
		}
		for {
			shell(check, &name, &sleep)
			time.Sleep(repeat)
		}
	}
}

// Shell worker.
func shell(check *Check, name *string, sleep *time.Duration) {
	// Execute with shell in N attemps.
	var out []byte
	var err error
	for i := 0; i < check.Tries; {
		out, err = exec.Command("sh", "-c", check.Shell).CombinedOutput()
		if err == nil {
			if check.Match != "" { // Match regexp.
				var regex *regexp.Regexp
				regex, err = regexp.Compile(check.Match)
				if err == nil && !regex.Match(out) {
					err = errors.New("Expected:\n" + check.Match + "\n\nGot:\n" + string(out))
				}
			}
			break
		}
		i++
		if i < check.Tries {
			time.Sleep(*sleep)
		}
	}
	// Process results.
	if err == nil {
		if check.Failed {
			ts := time.Now()
			mutex.Lock()
			check.Failed = false
			check.Since = ts.Format(time.RFC3339)
			modified = etag(ts)
			mutex.Unlock()
			subject := "Fixed: " + *name
			log(&subject, nil)
			if check.Notify != "" {
				notify(check, &subject, nil)
			}
			if check.Alert != "" {
				alert(check, name, nil, false)
			}
		}
	} else {
		if !check.Failed {
			ts := time.Now()
			mutex.Lock()
			check.Failed = true
			check.Since = ts.Format(time.RFC3339)
			modified = etag(ts)
			mutex.Unlock()
			msg := string(out) + err.Error()
			subject := "Failed: " + *name
			log(&subject, &msg)
			if check.Notify != "" {
				notify(check, &subject, &msg)
			}
			if check.Alert != "" {
				alert(check, name, &msg, true)
			}
		}
	}
}

// Web worker.
func web(check *Check, name *string, sleep *time.Duration) {
	// Get the URL in N attempts.
	var err error
	for i := 0; i < check.Tries; {
		err = fetch(check.Web, check.Match, check.Return)
		if err == nil {
			break
		}
		i++
		if i < check.Tries {
			time.Sleep(*sleep)
		}
	}
	// Process results.
	if err == nil {
		if check.Failed {
			ts := time.Now()
			mutex.Lock()
			check.Failed = false
			check.Since = ts.Format(time.RFC3339)
			modified = etag(ts)
			mutex.Unlock()
			subject := "Fixed: " + *name
			log(&subject, nil)
			if check.Notify != "" {
				notify(check, &subject, nil)
			}
			if check.Alert != "" {
				alert(check, name, nil, false)
			}
		}
	} else {
		if !check.Failed {
			ts := time.Now()
			mutex.Lock()
			check.Failed = true
			check.Since = ts.Format(time.RFC3339)
			modified = etag(ts)
			mutex.Unlock()
			msg := err.Error()
			subject := "Failed: " + *name
			log(&subject, &msg)
			if check.Notify != "" {
				notify(check, &subject, &msg)
			}
			if check.Alert != "" {
				alert(check, name, &msg, true)
			}
		}
	}
}

// Check HTTP redirects.
func redirect(req *http.Request, via []*http.Request) error {
	// When redirects number > 10 probably there's a problem.
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	// Redirects don't get User-Agent.
	req.Header.Set("User-Agent", "jsonmon")
	return nil
}

// The actual HTTP GET.
func fetch(url string, match string, code int) error {
	var err error
	var resp *http.Response
	var req *http.Request
	client := &http.Client{}
	client.CheckRedirect = redirect
	req, err = http.NewRequest("GET", url, nil)
	if err == nil {
		req.Header.Set("User-Agent", "jsonmon")
		resp, err = client.Do(req)
		if err == nil {
			if resp.StatusCode != code { // Check status code.
				err = errors.New(url + " returned " + strconv.Itoa(resp.StatusCode))
			} else { // Match regexp.
				if resp != nil && match != "" {
					var regex *regexp.Regexp
					regex, err = regexp.Compile(match)
					if err == nil {
						var body []byte
						body, _ = ioutil.ReadAll(resp.Body)
						if !regex.Match(body) {
							err = errors.New("Expected:\n" + match + "\n\nGot:\n" + string(body))
						}
					}
				}
			}
		}
	}
	if resp != nil {
		resp.Body.Close()
	}
	return err
}

// Logs status changes to stdout.
func log(subject *string, message *string) {
	// Log the alerts.
	if message == nil {
		fmt.Print("<5>", *subject, "\n")
	} else {
		fmt.Print("<5>", *subject, "\n", *message, "\n")
	}
}

// Mail notifications.
func notify(check *Check, subject *string, message *string) {
	// Make the message.
	var msg bytes.Buffer
	msg.WriteString("To: ")
	msg.WriteString(check.Notify)
	msg.WriteString("\nSubject: ")
	msg.WriteString(*subject)
	msg.WriteString("\nX-Mailer: jsonmon\n\n")
	if message != nil {
		msg.WriteString(*message)
	}
	msg.WriteString("\n.\n")
	// And send it.
	sendmail := exec.Command("/usr/sbin/sendmail", "-t")
	stdin, _ := sendmail.StdinPipe()
	err := sendmail.Start()
	if err != nil {
		fmt.Fprint(os.Stderr, "<3>", err, "\n")
	}
	io.WriteString(stdin, msg.String())
	sendmail.Wait()
}

// Executes callback. Passes args: true/false, check's name, message.
func alert(check *Check, name *string, msg *string, failed bool) {
	var out []byte
	var err error
	if msg != nil {
		out, err = exec.Command(check.Alert, strconv.FormatBool(failed), *name, *msg).CombinedOutput()
	} else {
		out, err = exec.Command(check.Alert, strconv.FormatBool(failed), *name).CombinedOutput()
	}
	if err != nil {
		fmt.Fprint(os.Stderr, "<3>", check.Alert, " failed\n", string(out), err.Error(), "\n")
	}
}

// Serve the Web UI.
func getUI(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Server", "jsonmon")
	switch r.URL.Path {
	case "/":
		displayUI(w, r, "text/html", "index.html", &modHTML)
	case "/angular.min.js":
		displayUI(w, r, "application/javascript", "angular.min.js", &modAngular)
	case "/app.js":
		displayUI(w, r, "application/javascript", "app.js", &modJS)
	case "/main.css":
		displayUI(w, r, "text/css", "main.css", &modCSS)
	default:
		http.NotFound(w, r)
	}
}

// Web UI caching and delivery.
func displayUI(w http.ResponseWriter, r *http.Request, mime string, name string, modified *string) {
	if cached := r.Header.Get("If-Modified-Since"); cached == *modified {
		w.WriteHeader(http.StatusNotModified)
	} else {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Last-Modified", *modified)
		h.Set("Content-Type", mime)
		data, _ := Asset(name)
		w.Write(data)
	}
}

// Display checks' details.
func getChecks(w http.ResponseWriter, r *http.Request) {
	displayJSON(w, r, &checks, &modified, true)
}

// Display application version.
func getVersion(w http.ResponseWriter, r *http.Request) {
	displayJSON(w, r, &version, &started, false)
}

// Output JSON.
func displayJSON(w http.ResponseWriter, r *http.Request, data interface{}, cache *string, lock bool) {
	var cached bool
	var result []byte
	h := w.Header()
	h.Set("Server", "jsonmon")
	if lock {
		mutex.RLock()
	}
	if r.Header.Get("If-None-Match") == *cache {
		cached = true
	} else {
		h.Set("ETag", *cache)
		result, _ = json.Marshal(&data)
	}
	if lock {
		mutex.RUnlock()
	}
	if cached {
		w.WriteHeader(http.StatusNotModified)
	} else {
		h.Set("Cache-Control", "no-cache")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Type", "application/json; charset=utf-8")
		w.Write(result)
	}
}
