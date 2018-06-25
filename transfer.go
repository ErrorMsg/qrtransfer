package main

import (
	"flag"
	"log"
	"net"
	"fmt"
	"net/http"
	"github.com/mdp/qrterminal"
	"os"
	"runtime"
	"github.com/mattn/go-colorable"
	"os/signal"
	"sync"
	"strings"
	"io/ioutil"
	"encoding/json"
	"os/user"
	"path/filepath"
	"github.com/jhoonb/archivex"
	"time"
	"regexp"
	"bufio"
	"strconv"
	"io"
	"encoding/base64"
	"crypto/rand"
	"errors"
	"context"
)

var zipFlag = flag.Bool("zip",false, "zip content")
var forceFlag = flag.Bool("force", false, "ignore saved config")
var debugFlag = flag.Bool("debug", false, "increase verbosity")
var quietFlag = flag.Bool("quiet", false, "ignore non-critical log")

func main(){
	flag.Parse()
	config := LoadConfig()
	if *forceFlag == true{
		config.Delete()
		config = LoadConfig()
	}
	if len(flag.Args()) == 0{
		log.Fatalln("at least one argument")
	}
	content, err := getContent(flag.Args())
	if err != nil {
		log.Fatalln(err)
	}
	address, err := getAddress(&config)
	fmt.Println(address)
	if err != nil{
		log.Fatalln(err)
	}
	listener, err := net.Listen("tcp", address + ":0")
	if err != nil{
		log.Fatalln(err)
	}
	address = fmt.Sprintf("%s:%d", address, listener.Addr().(*net.TCPAddr).Port)
	fmt.Println(address)
	randomPath := getRandomURLPath()
	generatedAddress := fmt.Sprintf("http://%s/%s", listener.Addr().String(), randomPath)
	fmt.Println(generatedAddress)
	info("Scan the following QR to start the download.")
	info("Make sure that your smartphone is connected to the same WiFi network as this computer.")
	info("Your generated address is", generatedAddress)

	qrConfig := qrterminal.Config{
		HalfBlocks:     true,
		Level:          qrterminal.L,
		Writer:         os.Stdout,
		BlackWhiteChar: "\u001b[37m\u001b[40m\u2584\u001b[0m",
		BlackChar:      "\u001b[30m\u001b[40m\u2588\u001b[0m",
		WhiteBlackChar: "\u001b[30m\u001b[47m\u2585\u001b[0m",
		WhiteChar:      "\u001b[37m\u001b[47m\u2588\u001b[0m",
	}
	if runtime.GOOS == "windows" {
		qrConfig.HalfBlocks = false
		qrConfig.Writer = colorable.NewColorableStdout()
		qrConfig.BlackChar = qrterminal.BLACK
		qrConfig.WhiteChar = qrterminal.WHITE
	}

	qrterminal.GenerateWithConfig(generatedAddress, qrConfig)

	// Create a server
	srv := &http.Server{Addr: address}

	// Create channel to send message to stop server
	stop := make(chan bool)

	// Wait for stop and then shutdown the server,
	go func() {
		<-stop
		if err := srv.Shutdown(context.Background()); err != nil {
			log.Println(err)
		}
	}()

	// Gracefully shutdown when an OS signal is received
	sig := make(chan os.Signal, 1)
	signal.Notify(sig)
	go func() {
		<-sig
		stop <- true
	}()

	// The handler adds and removes from the sync.WaitGroup
	// When the group is zero all requests are completed
	// and the server is shutdown
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wg.Wait()
		stop <- false
	}()

	// Create cookie used to verify request is coming from first client to connect
	cookie := http.Cookie{Name: "qr-filetransfer", Value: ""}

	var initCookie sync.Once

	// Define a default handler for the requests
	route := fmt.Sprintf("/%s", randomPath)
	http.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
		// If the cookie's value is empty this is the first connection
		// and the initialize the cookie.
		// Wrapped in a sync.Once to avoid potential race conditions
		if cookie.Value == "" {
			if !strings.HasPrefix(r.Header.Get("User-Agent"), "Mozilla") {
				http.Error(w, "", http.StatusOK)
				return
			}
			initCookie.Do(func() {
				value, err := getSessionID()
				if err != nil {
					log.Println("Unable to generate session ID", err)
					stop <- true
				}
				cookie.Value = value
				http.SetCookie(w, &cookie)
			})
		} else {
			// Check for the expected cookie and value
			// If it is missing or doesn't match
			// return a 404 status
			rcookie, err := r.Cookie(cookie.Name)
			if err != nil || rcookie.Value != cookie.Value {
				http.Error(w, "", http.StatusNotFound)
				return
			}
			// If the cookie exits and matches
			// this is an aadditional request.
			// Increment the waitgroup
			wg.Add(1)
		}

		defer wg.Done()
		w.Header().Set("Content-Disposition",
			"attachment; filename="+content.Name())
		http.ServeFile(w, r, content.Path)
	})

	// Enable TCP keepalives on the listener and start serving requests
	if err := (srv.Serve(tcpKeepAliveListener{listener.(*net.TCPListener)})); err != http.ErrServerClosed {
		log.Fatalln(err)
	}

	if content.ShouldBeDeleted {
		if err := content.Delete(); err != nil {
			log.Println("Unable to delete the content from disk", err)
		}
	}
	if err := config.Update(); err != nil {
		log.Println("Unable to update configuration", err)
	}

}


type Config struct {
	Iface string `json:"interface"`
}

func configFile() (string, error) {
	currentUser, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(currentUser.HomeDir, ".qr-filetransfer.json"), nil
}

// Update the configuration file
func (c *Config) Update() error {
	debug("Updating config file")
	j, err := json.Marshal(c)
	if err != nil {
		return err
	}
	file, err := configFile()
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(file, j, 0644)
	if err != nil {
		return err
	}
	return nil
}

// Delete the configuration file
func (c *Config) Delete() (bool, error) {
	file, err := configFile()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return false, nil
	}
	if err := os.Remove(file); err != nil {
		return false, err
	}
	return true, nil
}

// LoadConfig from file
func LoadConfig() Config {
	var config Config
	file, err := configFile()
	if err != nil {
		return config
	}
	debug("Current config file is", file)
	b, err := ioutil.ReadFile(file)
	if err != nil {
		return config
	}
	if err = json.Unmarshal(b, &config); err != nil {
		log.Println("WARN:", err)
	}
	return config
}


type Content struct {
	Path string
	// Should the content be deleted from disk after transfering? This is true
	// only if the content has been zipped by qr-filetransfer
	ShouldBeDeleted bool
}

// Name returns the base name of the content being transfered
func (c *Content) Name() string {
	return filepath.Base(c.Path)
}

// Delete the file from disk
func (c *Content) Delete() error {
	return os.Remove(c.Path)
}

// zipContent creates a new zip archive that stores the passed paths.
// It returns the path to the newly created zip file, and an error
func zipContent(args []string) (string, error) {
	fmt.Println("Adding the following items to a zip file:",
		strings.Join(args, " "))
	zip := new(archivex.ZipFile)
	tmpfile, err := ioutil.TempFile("", "qr-filetransfer")
	if err != nil {
		return "", err
	}
	tmpfile.Close()
	if err := os.Rename(tmpfile.Name(), tmpfile.Name()+".zip"); err != nil {
		return "", err
	}
	zip.Create(tmpfile.Name() + ".zip")
	for _, item := range args {
		f, err := os.Stat(item)
		if err != nil {
			return "", err
		}
		if f.IsDir() == true {
			zip.AddAll(item, true)
		} else {
			zip.AddFile(item)
		}
	}
	if err := zip.Close(); err != nil {
		return "", nil
	}
	return zip.Name, nil
}

// getContent returns an instance of Content and an error
func getContent(args []string) (Content, error) {
	content := Content{
		ShouldBeDeleted: false,
	}
	toBeZipped, err := shouldBeZipped(args)
	if err != nil {
		return content, err
	}
	if toBeZipped {
		content.ShouldBeDeleted = true
		content.Path, err = zipContent(args)
		if err != nil {
			return content, err
		}
	} else {
		content.Path = args[0]
	}
	return content, nil
}


type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (net.Conn, error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return nil, err
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(3 * time.Minute)
	return tc, nil
}


func debug(args ...string) {
	if *quietFlag == false && *debugFlag == true {
		log.Println(args)
	}
}

// info prints its argument if the -quiet flag is not passed
func info(args ...interface{}) {
	if *quietFlag == false {
		fmt.Println(args...)
	}
}

// findIP returns the IP address of the passed interface, and an error
func findIP(iface net.Interface) (string, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok {
			if ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String(), nil
			}
			return "[" + ipnet.IP.String() + "]", nil
		}
	}
	return "", errors.New("Unable to find an IP for this interface")
}

// getAddress returns the address of the network interface to
// bind the server to. The first time is run it prompts a
// dialog to choose which network interface should be used
// for the transfer
func getAddress(config *Config) (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	var candidateInterface *net.Interface
	for _, iface := range ifaces {
		if iface.Name == config.Iface {
			candidateInterface = &iface
			break
		}
	}
	if candidateInterface != nil {
		ip, err := findIP(*candidateInterface)
		if err != nil {
			return "", err
		}
		return ip, nil
	}

	var filteredIfaces []net.Interface
	var re = regexp.MustCompile(`^(veth|br\-|docker|lo|EHC|XHC|bridge|gif|stf|p2p|awdl|utun|tun|tap)`)
	for _, iface := range ifaces {
		if re.MatchString(iface.Name) {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		filteredIfaces = append(filteredIfaces, iface)
	}
	if len(filteredIfaces) == 0 {
		return "", errors.New("No network interface available.")
	}
	if len(filteredIfaces) == 1 {
		candidateInterface = &filteredIfaces[0]
		ip, err := findIP(*candidateInterface)
		if err != nil {
			return "", err
		}
		return ip, nil
	}
	fmt.Println("Choose the network interface to use (type the number):")
	for n, iface := range filteredIfaces {
		fmt.Printf("[%d] %s\n", n, iface.Name)
	}
	reader := bufio.NewReader(os.Stdin)
	text, _ := reader.ReadString('\n')
	index, err := strconv.Atoi(strings.Trim(text, "\n\r"))
	if err != nil {
		return "", err
	}
	if index+1 > len(filteredIfaces) {
		return "", errors.New("Wrong number")
	}
	candidateInterface = &filteredIfaces[index]
	ip, err := findIP(*candidateInterface)
	if err != nil {
		return "", err
	}
	config.Iface = candidateInterface.Name
	return ip, nil
}

// shouldBeZipped returns a boolean value indicating if the
// content should be zipped or not, and an error.
// The content should be zipped if:
// 1. the user passed the `-zip` flag
// 2. there are more than one file
// 3. the file is a directory
func shouldBeZipped(args []string) (bool, error) {
	if *zipFlag == true {
		return true, nil
	}
	if len(args) > 1 {
		return true, nil
	}
	file, err := os.Stat(args[0])
	if err != nil {
		return false, err
	}
	if file.IsDir() {
		return true, nil
	}
	return false, nil
}

// getRandomURLPath returns a random string of 4 alphanumeric characters
func getRandomURLPath() string {
	timeNum := time.Now().UTC().UnixNano()
	alphaString := strconv.FormatInt(timeNum, 36)
	return alphaString[len(alphaString)-4:]
}

// getSessionID returns a base64 encoded string of 40 random characters
func getSessionID() (string, error) {
	randbytes := make([]byte, 40)
	if _, err := io.ReadFull(rand.Reader, randbytes); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(randbytes), nil
}
