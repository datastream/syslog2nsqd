package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/consul/api"
	"github.com/jeromer/syslogparser/rfc3164"
	"github.com/nsqio/go-nsq"
	"log"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type StreamServer struct {
	*Setting
	exitChan      chan int
	msgChan       chan *LogFormat
	CurrentConfig map[string][]*regexp.Regexp
	wg            sync.WaitGroup
	client        *api.Client
	sync.Mutex
}

func (s *StreamServer) Run() {
	cfg := nsq.NewConfig()
	hostname, _ := os.Hostname()
	cfg.Set("user_agent", fmt.Sprintf("netstream/%s", hostname))
	cfg.Set("snappy", true)
	for i := 0; i < s.WritePoolSize; i++ {
		w, _ := nsq.NewProducer(s.NsqdAddr, cfg)
		go s.writeLoop(w)
	}
	ticker := time.Tick(time.Second * 600)
	go s.readUDP()
	go s.readTCP()
	var err error
	config := api.DefaultConfig()
	config.Address = s.ConsulAddress
	config.Datacenter = s.Datacenter
	config.Token = s.Token
	s.client, err = api.NewClient(config)
	if err != nil {
		fmt.Println("reload consul setting failed", err)
	}
	s.CurrentConfig, err = s.GetRegexp()
	for {
		select {
		case <-ticker:
			s.Lock()
			s.CurrentConfig, err = s.GetRegexp()
			s.Unlock()
			if err != nil {
				fmt.Println("reload consul setting failed", err)
			}
		case <-s.exitChan:
			return
		}
	}
}

func (s *StreamServer) writeLoop(w *nsq.Producer) {
	var bodies [][]byte
	for {
		select {
		case msg := <-s.msgChan:
			if s.IsIgnoreLog([]byte(msg.GetRawmsg())) {
				break
			}
			record, err := proto.Marshal(msg)
			if err != nil {
				break
			}
			bodies = append(bodies, record)
			if len(bodies) > 20 {
				w.MultiPublish(s.Topic, bodies)
				bodies = bodies[:0]
			}
		case <-s.exitChan:
			return
		}
	}
}
func (s *StreamServer) Stop() {
	close(s.exitChan)
	s.wg.Wait()
}

func (s *StreamServer) readUDP() {
	udpAddr, err := net.ResolveUDPAddr("udp", s.UdpPort)
	if err != nil {
		log.Fatal("udp:", err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Fatal("server bind failed:", err)
	}
	defer conn.Close()
	buf := make([]byte, 8192*8)
	for {
		select {
		case <-s.exitChan:
			return
		default:
			size, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				log.Println("read log failed", err)
				continue
			}
			logFormat := &LogFormat{
				From:   proto.String(addr.String()),
				Rawmsg: proto.String(string(buf[:size])),
			}
			s.msgChan <- logFormat
		}
	}
}
func (s *StreamServer) readTCP() {
	server, err := net.Listen("tcp", s.TcpPort)
	if err != nil {
		log.Fatal("server bind failed:", err)
	}
	defer server.Close()
	for {
		select {
		case <-s.exitChan:
			return
		default:
			fd, err := server.Accept()
			if err != nil {
				log.Fatal("accept error", err)
				time.Sleep(time.Second)
			} else {
				go s.loghandle(fd)
			}
		}
	}
}

// receive log from tcp socket, encode json and send to msg_chan
func (s *StreamServer) loghandle(fd net.Conn) {
	defer fd.Close()
	scanner := bufio.NewScanner(fd)
	scanner.Split(bufio.ScanLines)
	addr := fd.RemoteAddr()
	s.wg.Add(1)
	defer s.wg.Done()
	var err error
	for {
		select {
		case <-s.exitChan:
			return
		default:
			if scanner.Scan() == false {
				err = scanner.Err()
				if err == nil {
					return
				}
			}
			if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
				log.Println(err)
				return
			}
			line := scanner.Text()
			logFormat := &LogFormat{
				From:   proto.String(addr.String()),
				Rawmsg: proto.String(string(line)),
			}
			s.msgChan <- logFormat
		}
	}
}

func (s *StreamServer) IsIgnoreLog(buf []byte) bool {
	if len(buf) < 1 {
		return true
	}
	p := rfc3164.NewParser(buf)
	if err := p.Parse(); err != nil {
		return false
	}
	data := p.Dump()
	tag := data["tag"].(string)
	if len(tag) == 0 {
		return false
	}
	s.Lock()
	rgs, ok := s.CurrentConfig[tag]
	s.Unlock()
	if ok {
		for _, r := range rgs {
			if r.MatchString(data["content"].(string)) {
				return true
			}
		}
	}
	return false
}
func (s *StreamServer) GetRegexp() (map[string][]*regexp.Regexp, error) {
	consulSetting := make(map[string][]*regexp.Regexp)
	kv := s.client.KV()
	pairs, _, err := kv.List(s.ConsulKey, nil)
	if err != nil {
		return consulSetting, err
	}
	size := len(s.ConsulKey) + 1
	for _, value := range pairs {
		if len(value.Key) > size {
			var regs []string
			if err := json.Unmarshal(value.Value, &regs); err == nil {
				var rs []*regexp.Regexp
				for _, v := range regs {
					x, e := regexp.CompilePOSIX(v)
					if e != nil {
						log.Println("get regexp", e)
						continue
					}
					rs = append(rs, x)
				}
				consulSetting[value.Key[size:]] = rs
			}
		}
	}
	return consulSetting, err
}
