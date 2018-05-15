package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/itchio/wharf/eos/option"

	"github.com/itchio/httpkit/htfs"
	"github.com/itchio/wharf/wrand"
	"github.com/pkg/errors"

	"github.com/itchio/wharf/eos"
)

type fakeFileSystem struct {
	fakeData []byte
}

func (ffs *fakeFileSystem) Open(name string) (http.File, error) {
	br := bytes.NewReader(ffs.fakeData)
	ff := &fakeFile{
		Reader: br,
		FS:     ffs,
	}
	return ff, nil
}

type fakeFile struct {
	*bytes.Reader
	FS *fakeFileSystem
}

func (ff *fakeFile) Stat() (os.FileInfo, error) {
	return &fakeStats{fakeFile: ff}, nil
}

func (ff *fakeFile) Readdir(count int) ([]os.FileInfo, error) {
	return nil, nil
}

func (ff *fakeFile) Close() error {
	return nil
}

type fakeStats struct {
	fakeFile *fakeFile
}

func (fs *fakeStats) Name() string {
	return "bin.dat"
}

func (fs *fakeStats) IsDir() bool {
	return false
}

func (fs *fakeStats) Size() int64 {
	return int64(len(fs.fakeFile.FS.fakeData))
}

func (fs *fakeStats) Mode() os.FileMode {
	return 0644
}

func (fs *fakeStats) ModTime() time.Time {
	return time.Now()
}

func (fs *fakeStats) Sys() interface{} {
	return nil
}

func main() {
	must(doMain())
}

type delayHandler struct {
	realHandler http.Handler
}

func (dh *delayHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	time.Sleep(time.Millisecond * time.Duration(10+rand.Intn(80)))
	dh.realHandler.ServeHTTP(w, req)
}

func doMain() error {
	log.Printf("Generating fake data...")
	prng := &wrand.RandReader{
		Source: rand.NewSource(time.Now().UnixNano()),
	}
	var fakeDataSize int64 = 32 * 1024 * 1024
	fakeData, err := ioutil.ReadAll(io.LimitReader(prng, fakeDataSize))
	must(err)

	http.Handle("/", http.FileServer(&fakeFileSystem{fakeData}))

	log.Printf("Starting http server...")
	l, err := net.Listen("tcp", "localhost:0")
	must(err)

	go func() {
		log.Fatal(http.Serve(l, nil))
	}()

	url := fmt.Sprintf("http://%s/file.dat", l.Addr().String())

	f, err := eos.Open(url, option.WithHTFSDumpStats())
	must(err)
	defer f.Close()

	done := make(chan bool)
	numErrors := 0

	printInterval := 250
	readsPerWorker := 3000 * 1000

	const (
		actionForward = iota
		actionSeekForwardLittle
		actionSeekBackLittle
		actionSeekForwardLarge
		actionSeekBackLarge
		actionReset
	)

	sigChan := make(chan os.Signal)
	signal.Notify(sigChan, syscall.SIGINT)

	var running int64 = 1
	var totalReads int64
	startTime := time.Now()

	numWorkers := 4
	work := func(workerNum int) {
		defer func() {
			done <- true
		}()

		var action = actionForward

		var lastOffset int64
		var lastN int64

		source := rand.NewSource(time.Now().UnixNano())
		buf := make([]byte, 739+2000)

		for i := 1; i < readsPerWorker; i++ {
			if atomic.LoadInt64(&running) != 1 {
				log.Printf("[%d] winding down...", workerNum)
				return
			}

			newTotalReads := atomic.AddInt64(&totalReads, 1)

			if newTotalReads%int64(printInterval) == 0 {
				hf := f.(*htfs.File)
				hf.NumConns()
				log.Printf("%d reads... (%d workers, %d conns, running for %s)", newTotalReads, numWorkers, hf.NumConns(), time.Since(startTime))
			}

			x := source.Int63() % 100
			switch {
			case x < 80:
				action = actionForward
			case x < 90:
				action = actionSeekForwardLittle
			case x < 95:
				action = actionSeekBackLittle
			case x < 97:
				action = actionSeekForwardLarge
			default:
				action = actionSeekBackLarge
			}

			if lastOffset > int64(len(fakeData)-8*1024) {
				action = actionReset
			}

			var offset int64
			var readSize int64

			switch action {
			case actionForward:
				offset = lastOffset + lastN
			case actionSeekForwardLittle:
				offset = lastOffset + lastN + source.Int63()%1024
			case actionSeekBackLittle:
				offset = lastOffset + lastN - source.Int63()%1024
			case actionSeekForwardLarge:
				offset = lastOffset + lastN + source.Int63()%(fakeDataSize/4)
			case actionSeekBackLarge:
				offset = lastOffset + lastN - source.Int63()%(fakeDataSize/4)
			case actionReset:
				offset = 0
			}

			if offset >= int64(len(fakeData)-1) {
				offset = int64(len(fakeData) - 2)
			}
			if offset < 0 {
				offset = 0
			}
			readSize = 1 + (source.Int63() % int64(len(buf)-1))

			if offset+readSize > int64(len(fakeData)) {
				readSize = int64(len(fakeData)) - offset
			}

			n, err := f.ReadAt(buf[:readSize], offset)
			must(err)

			if !bytes.Equal(buf[:n], fakeData[offset:offset+int64(n)]) {
				log.Printf("%d read at %d did not match", n, offset)
				numErrors++
			}

			lastOffset = offset
			lastN = int64(n)
		}
	}

	for i := 0; i < numWorkers; i++ {
		go work(i)
	}

	for i := 0; i < numWorkers; i++ {
		select {
		case <-done:
			// cool
		case <-sigChan:
			atomic.StoreInt64(&running, 0)
		}
	}

	log.Printf("%d errors total", numErrors)
	if numErrors > 0 {
		return errors.Errorf("Had %d (> 0) errors", numErrors)
	}
	return nil
}

func must(err error) {
	if err != nil {
		panic(fmt.Sprintf("%+v", err))
	}
}
