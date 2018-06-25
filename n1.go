package main

import(
	"flag"
	"log"
	"net"
	"fmt"
	"net/http"
	"github.com/mdp/qrterminal"
	"os"
	_ "os/signal"
	"sync"
	"strings"
	"io/ioutil"
	"encoding/json"
	"os/user"
	"path/filepath"
	"github.com/jhoonb/archivex"
	"time"
	"strconv"
	"errors"
	"context"
	"bufio"
	"runtime"
	"github.com/mattn/go-colorable"
)

var zipFlag = flag.Bool("zip", false, "zip content")

func main(){
	flag.Parse()
	if len(flag.Args()) < 1{
		log.Panic("at least one argument")
	}
	config := loadConfig()
	content := getContent(flag.Args())
	address,_ := getAddress(config)
	ln, err := net.Listen("tcp", address+":0")
	if err != nil{
		log.Println(err)
	}
	address = fmt.Sprintf("%s:%d", address, ln.Addr().(*net.TCPAddr).Port)
	randomPath := getRandom()
	generatedAddress := fmt.Sprintf("http://%s/%s", ln.Addr().String(), randomPath)
	fmt.Println(generatedAddress)
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
	
	srv := &http.Server{Addr: address}
	stop := make(chan bool)
	go func(){
		<- stop
		if err:=srv.Shutdown(context.Background());err!=nil{
			log.Println(err)
		}
	}()
	
	//sig := make(chan os.Signal, 1)
	//signal.Notify(sig)
	go func(){
		//<- sig
		time.Sleep(time.Minute*1)
		stop <- true
	}()
	
	var wg sync.WaitGroup
	wg.Add(1)
	go func(){
		wg.Wait()
		stop <- true
	}()
	
	route := fmt.Sprintf("/%s", randomPath)
	http.HandleFunc(route, func(w http.ResponseWriter, r *http.Request){
		w.Header().Set("Content-Disposition", "attachment; filename=" + filepath.Base(content.Path))
		http.ServeFile(w, r, content.Path)
		defer wg.Done()
	})
	if err := (srv.Serve(tcpKeepAlive{ln.(*net.TCPListener)}));err != http.ErrServerClosed{
		log.Fatalln(err)
	}
}

type Config struct{
	Interface string `json:"interface"`
}

func configFile() (string, error){
	currentUser,err := user.Current()
	if err != nil{
		return "", err
	}
	return filepath.Join(currentUser.HomeDir, ".transfer.json"), nil
}

func loadConfig() Config{
	file,err := configFile()
	if err != nil{
		log.Println(err)
	}
	var config Config
	data,_ := ioutil.ReadFile(file)
	err = json.Unmarshal(data, &config)
	if err != nil{
		log.Println(err)
	}
	return config
}

func (c Config) update() error{
	data,err := json.Marshal(c)
	if err != nil{
		return err
	}
	file, err := configFile()
	if err != nil{
		return err
	}
	err = ioutil.WriteFile(file, data, 0644)
	if err != nil{
		return err
	}
	return nil
}

type Content struct{
	Path string
}

func getContent(args []string) Content{
	var content Content
	file,_ := os.Stat(args[0])
	if len(args) > 1 || *zipFlag == true || file.IsDir(){
		content.Path,_ = zipContent(args)
		return content
	}else{
		content.Path = args[0]
	}
	return content
}

func zipContent(args []string) (string, error){
	zip := new(archivex.ZipFile)
	tmpfile, err := ioutil.TempFile("", "transfer")
	if err != nil{
		return "", err
	}
	err = os.Rename(tmpfile.Name(), tmpfile.Name() + ".zip")
	if err != nil{
		return "", err
	}
	zip.Create(tmpfile.Name() + ".zip")
	for _, item := range args{
		file,_ := os.Stat(item)
		if file.IsDir(){
			zip.AddAll(item, true)
		}else{
			zip.AddFile(item)
		}
	}
	return zip.Name, nil
}

func getAddress(config Config) (string,error){
	var trueIface *net.Interface
	ifaces,err := net.Interfaces()
	if err != nil{
		return "", err
	}
	for _, iface := range ifaces{
		if iface.Name == config.Interface{
			trueIface = &iface
			break
		}
	}
	if trueIface != nil{
		ip, err := findIP(*trueIface)
		if err != nil{
			return "", err
		}
		return ip, nil
	}
	for i, iface := range ifaces{
		fmt.Printf("[%d] %s\n", i, iface.Name)
	}
	r := bufio.NewReader(os.Stdin)
	w,_ := r.ReadString('\n')
	i,_ := strconv.Atoi(strings.Trim(w, "\n\r"))
	if i+1 > len(ifaces){
		return "", errors.New("not found")
	}
	trueIface = &ifaces[i]
	ip,err := findIP(*trueIface)
	if err != nil{
		return "", err
	}
	config.Interface = trueIface.Name
	config.update()
	return ip, nil
}

func findIP(iface net.Interface) (string, error){
	addrs,err := iface.Addrs()
	if err != nil{
		return "", err
	}
	for _,addr := range addrs{
		if ipnet, ok := addr.(*net.IPNet); ok{
			if ipnet.IP.IsLinkLocalUnicast(){
				continue
			}
			if ipnet.IP.To4 != nil{
				return ipnet.IP.String(), nil
			}else{
				return string("[") + ipnet.IP.String() + string("]"), nil
			}
		}
	}
	return "", errors.New("no ip found")
}

type tcpKeepAlive struct{
	*net.TCPListener
}

func (ln tcpKeepAlive)Accept() (net.Conn, error){
	conn, err := ln.AcceptTCP()
	if err != nil{
		return nil, err
	}
	conn.SetKeepAlive(true)
	conn.SetKeepAlivePeriod(1*time.Minute)
	return conn, nil
}

func getRandom() string{
	seed := time.Now().Unix()
	str := strconv.FormatInt(seed, 36)
	return str[len(str)-4:]
}