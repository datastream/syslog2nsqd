package main

import (
	"bufio"
	"github.com/nsqio/go-nsq"
	"github.com/fsnotify/fsnotify"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type FileList struct {
	Files        map[string]bool
	FileDescribe map[string]*os.File
	FileStat     map[string]chan int
	sync.Mutex
}

func (f *FileList) Update(e fsnotify.Event) bool {
	switch e.Op {
	case fsnotify.Remove:
		f.Lock()
		delete(f.Files, e.Name)
		close(f.FileStat[e.Name])
		f.Unlock()
	case fsnotify.Write:
		if _, ok := f.Files[e.Name]; !ok {
			f.Lock()
			f.Files[e.Name] = true
			f.FileStat[e.Name] = make(chan int)
			f.Unlock()
			return true
		}
	default:
	}
	return false
}
func (f *FileList) ReadLog(file string, topic string, w *nsq.Producer, exitchan chan int) {
	items := strings.Split(file, "/")
	fileName := items[len(items)-1]
	if len(*filePatten) > 0 {
		re, err := regexp.Compile(*filePatten)
		if err != nil {
			return
		}
		if !re.MatchString(fileName) {
			return
		}
	}
	f.Lock()
	stat := true
	log.Println(file)
	fd, err := os.Open(file)
	if err != nil {
		log.Println(err)
		return
	}
	defer fd.Close()
	f.FileDescribe[file] = fd
	donechan := f.FileStat[file]
	f.Unlock()
	reader := bufio.NewReader(fd)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			time.Sleep(time.Second * 10)
			line, err = reader.ReadString('\n')
		}
		select {
		case <-exitchan:
			return
		case <-donechan:
			stat = false
		default:
		}
		if stat == false && err == io.EOF {
			return
		}
		if err == io.EOF {
			continue
		}
		err = w.Publish(topic, []byte(line))
		if err != nil {
			log.Println("NSQ writer", err)
		}
	}
}
