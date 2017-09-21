package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codeskyblue/kexec"
	"github.com/franela/goreq"
	"github.com/gorilla/mux"
)

// Get preferred outbound ip of this machine
func getOutboundIP() (ip net.IP, err error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP, nil
}

func mustGetOoutboundIP() net.IP {
	ip, err := getOutboundIP()
	if err != nil {
		panic(err)
	}
	return ip
}

func GoFunc(f func() error) chan error {
	ch := make(chan error)
	go func() {
		ch <- f()
	}()
	return ch
}

type MinicapInfo struct {
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Rotation int     `json:"rotation"`
	Density  float32 `json:"density"`
}

func runShell(args ...string) (output []byte, err error) {
	return exec.Command("sh", "-c", strings.Join(args, " ")).Output()
}

func httpDownload(path string, urlStr string, perms os.FileMode) (written int64, err error) {
	resp, err := goreq.Request{
		Uri:             urlStr,
		RedirectHeaders: true,
		MaxRedirects:    10,
	}.Do()
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("http download <%s> status %v", urlStr, resp.Status)
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, perms)
	if err != nil {
		return
	}
	defer file.Close()
	return io.Copy(file, resp.Body)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func InstallRequirements() error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if fileExists("/data/local/tmp/minicap") && fileExists("/data/local/tmp/minicap.so") && Screenshot("/dev/null") == nil {
		return nil
	}
	minicapSource := "https://github.com/codeskyblue/stf-binaries/raw/master/node_modules/minicap-prebuilt/prebuilt"
	propOutput, err := runShell("getprop")
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`\[(.*?)\]:\s*\[(.*?)\]`)
	matches := re.FindAllStringSubmatch(string(propOutput), -1)
	props := make(map[string]string)
	for _, m := range matches {
		var key = m[1]
		var val = m[2]
		props[key] = val
	}
	abi := props["ro.product.cpu.abi"]
	sdk := props["ro.build.version.sdk"]
	pre := props["ro.build.version.preview_sdk"]
	if pre != "" && pre != "0" {
		sdk = sdk + pre
	}
	binURL := strings.Join([]string{minicapSource, abi, "bin", "minicap"}, "/")
	_, err = httpDownload("/data/local/tmp/minicap", binURL, 0755)
	if err != nil {
		return err
	}
	libURL := strings.Join([]string{minicapSource, abi, "lib", "android-" + sdk, "minicap.so"}, "/")
	_, err = httpDownload("/data/local/tmp/minicap.so", libURL, 0644)
	if err != nil {
		return err
	}
	return nil
}

func Screenshot(filename string) (err error) {
	output, err := runShell("LD_LIBRARY_PATH=/data/local/tmp", "/data/local/tmp/minicap", "-i")
	if err != nil {
		return
	}
	var f MinicapInfo
	if er := json.Unmarshal([]byte(output), &f); er != nil {
		err = fmt.Errorf("minicap not supported: %v", er)
		return
	}
	if _, err = runShell(
		"LD_LIBRARY_PATH=/data/local/tmp",
		"/data/local/tmp/minicap",
		"-P", fmt.Sprintf("%dx%d@%dx%d/%d", f.Width, f.Height, f.Width, f.Height, f.Rotation),
		"-s", ">"+filename); err != nil {
		return
	}
	return nil
}

func safeRunUiautomator() {
	runUiautomator()
}

func runUiautomator() error {
	c := exec.Command("am", "instrument", "-w", "-r",
		"-e", "debug", "false",
		"-e", "class", "com.github.uiautomator.stub.Stub",
		"com.github.uiautomator.test/android.support.test.runner.AndroidJUnitRunner")
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

type DownloadManager struct {
	db map[string]*DownloadProxy
	mu sync.Mutex
	n  int
}

func newDownloadManager() *DownloadManager {
	return &DownloadManager{
		db: make(map[string]*DownloadProxy, 10),
	}
}

func (m *DownloadManager) Get(id string) *DownloadProxy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.db[id]
}

func (m *DownloadManager) Put(di *DownloadProxy) (id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n += 1
	id = strconv.Itoa(m.n)
	m.db[id] = di
	di.Id = id
	return id
}

func (m *DownloadManager) Del(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.db, id)
}

func (m *DownloadManager) DelayDel(id string, sleep time.Duration) {
	go func() {
		time.Sleep(sleep)
		m.Del(id)
	}()
}

type DownloadProxy struct {
	writer     io.Writer
	Id         string `json:"id"`
	TotalSize  int    `json:"titalSize"`
	CopiedSize int    `json:"copiedSize"`
	Error      string `json:"error,omitempty"`
	wg         sync.WaitGroup
}

func newDownloadProxy(wr io.Writer) *DownloadProxy {
	di := &DownloadProxy{
		writer: wr,
	}
	di.wg.Add(1)
	return di
}

func (d *DownloadProxy) Write(data []byte) (int, error) {
	n, err := d.writer.Write(data)
	d.CopiedSize += n
	return n, err
}

// Should only call once
func (d *DownloadProxy) Done() {
	d.wg.Done()
}

func (d *DownloadProxy) Wait() {
	d.wg.Wait()
}

var downManager = newDownloadManager()

func AsyncDownloadTo(url string, filepath string, autoRelease bool) (di *DownloadProxy, err error) {
	res, err := goreq.Request{
		Uri:             url,
		MaxRedirects:    10,
		RedirectHeaders: true,
	}.Do()
	if err != nil {
		return
	}
	file, err := os.Create(filepath)
	if err != nil {
		res.Body.Close()
		return
	}
	di = newDownloadProxy(file)
	fmt.Sscanf(res.Header.Get("Content-Length"), "%d", &di.TotalSize)
	downloadKey := downManager.Put(di)
	go func() {
		if autoRelease {
			defer downManager.Del(downloadKey)
		}
		defer di.Done()
		defer res.Body.Close()
		defer file.Close()
		io.Copy(di, res.Body)
	}()
	return
}

func ServeHTTP(port int) error {
	m := mux.NewRouter()

	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "Hello World!")
	})

	m.HandleFunc("/shell", func(w http.ResponseWriter, r *http.Request) {
		command := r.FormValue("command")
		if command == "" {
			command = r.FormValue("c")
		}
		output, err := exec.Command("sh", "-c", command).CombinedOutput()
		log.Println(err)
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"output": string(output),
		})
	})

	m.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "Finished!")
		go func() {
			time.Sleep(100 * time.Millisecond)
			os.Exit(0)
		}()
	})

	m.HandleFunc("/screenshot", func(w http.ResponseWriter, r *http.Request) {
		imagePath := "/data/local/tmp/minicap-screenshot.jpg"
		if err := Screenshot(imagePath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.ServeFile(w, r, imagePath)
	}).Methods("GET")

	m.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		url := r.FormValue("url")
		filepath := r.FormValue("filepath")
		res, err := goreq.Request{
			Uri:             url,
			MaxRedirects:    10,
			RedirectHeaders: true,
		}.Do()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		file, err := os.Create(filepath)
		if err != nil {
			res.Body.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		di := newDownloadProxy(file)
		fmt.Sscanf(res.Header.Get("Content-Length"), "%d", &di.TotalSize)
		downloadKey := downManager.Put(di)
		go func() {
			defer downManager.Del(downloadKey)
			defer res.Body.Close()
			defer file.Close()
			io.Copy(di, res.Body)
		}()
		io.WriteString(w, downloadKey)
	})

	m.HandleFunc("/uploadStats", func(w http.ResponseWriter, r *http.Request) {
		key := r.FormValue("key")
		di := downManager.Get(key)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(di)
	}).Methods("GET")

	m.HandleFunc("/install", func(w http.ResponseWriter, r *http.Request) {
		url := r.FormValue("url")
		filepath := r.FormValue("filepath")
		if filepath == "" {
			filepath = "/sdcard/tmp.apk"
		}
		di, err := AsyncDownloadTo(url, filepath, false) // use false to disable DownloadProxy auto recycle
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go func() {
			di.Wait() // wait download finished
			if runtime.GOOS == "windows" {
				log.Println("fake pm install")
				downManager.Del(di.Id)
				return
			}
			// -g: grant all runtime permissions
			output, err := runShell("pm", "install", "-r", "-g", filepath)
			if err != nil {
				di.Error = err.Error() + " >> " + string(output)
			}
			downManager.DelayDel(di.Id, time.Minute*5)
		}()
		io.WriteString(w, di.Id)
	}).Methods("POST")

	m.HandleFunc("/install/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]
		dp := downManager.Get(id)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(dp)
	})

	m.HandleFunc("/upgrade", func(w http.ResponseWriter, r *http.Request) {
		ver := r.FormValue("version")
		var err error
		if ver == "" {
			ver, err = getLatestVersion()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		err = doUpdate(ver)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		io.WriteString(w, "update finished, restarting")
		go runDaemon()
	})

	return http.ListenAndServe(":"+strconv.Itoa(port), m)
}

func runDaemon() {
	environ := os.Environ()
	environ = append(environ, "IGNORE_SIGHUP=true")
	cmd := kexec.Command(os.Args[0], "-p", strconv.Itoa(listenPort))
	cmd.Env = environ
	cmd.Start()
	select {
	case err := <-GoFunc(cmd.Wait):
		log.Fatalf("server started failed, %v", err)
	case <-time.After(200 * time.Millisecond):
		fmt.Printf("server started, listening on %v:%d\n", mustGetOoutboundIP(), listenPort)
	}
}

func main() {
	daemon := flag.Bool("d", false, "run daemon")
	flag.IntVar(&listenPort, "p", 7912, "listen port") // Create on 2017/09/12
	showVersion := flag.Bool("v", false, "show version")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	log.Println("Check environment")
	if err := InstallRequirements(); err != nil {
		panic(err)
	}

	if *daemon {
		runDaemon()
	}

	if os.Getenv("IGNORE_SIGHUP") == "true" {
		fmt.Println("Enter into daemon mode")
		f, err := os.Create("/sdcard/atx-agent.log")
		if err != nil {
			panic(err)
		}
		defer f.Close()
		log.SetOutput(f)
		log.Println("Ignore SIGUP")
		signal.Ignore(syscall.SIGHUP)

		// kill previous daemon first
		_, err = http.Get(fmt.Sprintf("http://localhost:%d/stop", listenPort))
		if err == nil {
			log.Println("wait previous server stopped")
			time.Sleep(500 * time.Millisecond) // server will quit in 0.1s
		}
	}

	// show ip
	outIp, err := getOutboundIP()
	if err == nil {
		fmt.Printf("IP: %v\n", outIp)
	} else {
		fmt.Printf("Internet is not connected.")
	}

	go safeRunUiautomator()
	log.Fatal(ServeHTTP(listenPort))
}