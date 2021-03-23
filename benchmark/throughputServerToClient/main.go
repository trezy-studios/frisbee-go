package main

import (
	"crypto/rand"
	"fmt"
	"github.com/loophole-labs/frisbee"
	"github.com/loophole-labs/frisbee/internal/protocol"
	"github.com/loophole-labs/frisbee/pkg/client"
	"github.com/loophole-labs/frisbee/pkg/server"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"io/ioutil"
	"time"
)

const testSize = 1000
const messageSize = 512
const runs = 10
const port = 8192

var complete = make(chan struct{})

func main() {
	serverRouter := make(frisbee.ServerRouter)
	serverRouter[protocol.MessagePong] = func(_ frisbee.Conn, incomingMessage frisbee.Message, _ []byte) (outgoingMessage *frisbee.Message, outgoingContent []byte, action frisbee.Action) {
		if incomingMessage.Id == testSize {
			complete <- struct{}{}
		}
		return
	}

	sum := uint32(testSize * (testSize + 1) / 2)
	receiveSum := uint32(0)

	clientRouter := make(frisbee.ClientRouter)
	clientRouter[protocol.MessagePing] = func(incomingMessage frisbee.Message, _ []byte) (outgoingMessage *frisbee.Message, outgoingContent []byte, action frisbee.Action) {
		receiveSum += incomingMessage.Id
		if receiveSum == sum {
			outgoingMessage = &frisbee.Message{
				Id:            testSize,
				Operation:     protocol.MessagePong,
				Routing:       0,
				ContentLength: 0,
			}
			receiveSum = 0
		}
		//log.Printf("Receive Sum: %d, id: %d", receiveSum, incomingMessage.Id)
		return
	}

	var benchmarkConnection frisbee.Conn
	connected := make(chan struct{})

	emptyLogger := zerolog.New(ioutil.Discard)
	s := server.NewServer(fmt.Sprintf(":%d", port), serverRouter, server.WithAsync(true), server.WithLogger(&emptyLogger), server.WithMulticore(true), server.WithLoops(16))
	s.UserOnOpened = func(s *server.Server, c frisbee.Conn) frisbee.Action {
		benchmarkConnection = c
		connected <- struct{}{}
		return frisbee.None
	}

	s.Start()

	c := client.NewClient(fmt.Sprintf("127.0.0.1:%d", port), clientRouter, client.WithLogger(&emptyLogger))
	_ = c.Connect()

	data := make([]byte, messageSize)
	_, _ = rand.Read(data)

	duration := time.Nanosecond
	<-connected
	for i := 1; i < runs+1; i++ {
		start := time.Now()
		for q := 1; q < testSize+1; q++ {
			err := benchmarkConnection.Write(frisbee.Message{
				Id:            uint32(q),
				Operation:     protocol.MessagePing,
				Routing:       uint32(i),
				ContentLength: messageSize,
			}, &data)
			if err != nil {
				panic(err)
			}
		}
		<-complete
		runTime := time.Since(start)
		log.Printf("Benchmark Time for test %d: %s", i, runTime)
		duration += runTime
	}
	log.Printf("Average Benchmark time for %d runs: %s", runs, duration/runs)
	_ = s.Stop()
	_ = c.Stop()
}