package main

import (
	"github.com/loophole-labs/frisbee"
	"github.com/rs/zerolog/log"
	"hash/crc32"
	"os"
	"os/signal"
)

const PUB = uint16(1)
const SUB = uint16(2)

var topic = []byte("SENDING")
var topicHash = crc32.ChecksumIEEE(topic)

const END = "END"

// Handle the PUB message type
func handlePub(incomingMessage frisbee.Message, incomingContent []byte) (outgoingMessage *frisbee.Message, outgoingContent []byte, action frisbee.Action) {
	if incomingMessage.Routing == topicHash {
		log.Printf("Client Received Message on Topic %s: %s", string(topic), string(incomingContent))
		if string(incomingContent) == END {
			outgoingMessage = &frisbee.Message{}
		}
	}
	return
}

func main() {
	router := make(frisbee.ClientRouter)
	router[PUB] = handlePub
	exit := make(chan os.Signal, 1)
	signal.Notify(exit, os.Interrupt)

	c := frisbee.NewClient("127.0.0.1:8192", router)
	err := c.Connect()
	if err != nil {
		panic(err)
	}

	i := 0

	// First subscribe to the topic
	err = c.Write(&frisbee.Message{
		Id:            uint32(i),
		Operation:     SUB,
		Routing:       0,
		ContentLength: uint32(len(topic)),
	}, &topic)
	if err != nil {
		panic(err)
	}

	// Now the handle pub function will be called
	// automatically whenever a message that matches the topic arrives

	<-exit
	err = c.Close()
	if err != nil {
		panic(err)
	}
}
