package socketio

import (
	"flag"

	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

const (
	SERVER_ADDR  = "localhost"
	PORT         = "9999"
	NUM_MESSAGES = 10000
)

func startServer(t *testing.T) {

	config := DefaultConfig
	config.QueueLength = 1000
	config.ReconnectTimeout = 1e6
	config.Origins = []string{"*"}

	server := NewSocketIO(&config)

	server.OnMessage(func(c *Conn, msg Message) {
		if err := c.Send(msg.Data()); err != nil {
			t.Logf("server echo send error: ", err)
		}
	})

	go func() {
		http.ListenAndServe(fmt.Sprintf("%s:%s", SERVER_ADDR, PORT), server.ServeMux())
	}()

}

func TestStressTest(t *testing.T) {

	clients := 1
	passes := 1
	msg_size := 150 // bytes
	numMessages := NUM_MESSAGES
	serverAddr := fmt.Sprintf("%s:%s", SERVER_ADDR, PORT)

	flag.Parse()
	if len(flag.Args()) > 0 {
		serverAddr = flag.Arg(0)
	}

	if v, err := strconv.Atoi(flag.Arg(1)); err == nil {
		passes = v
	}

	if v, err := strconv.Atoi(flag.Arg(2)); err == nil {
		clients = v
	}

	if v, err := strconv.Atoi(flag.Arg(3)); err == nil {
		if v > msg_size {
			msg_size = v
		}
	}

	if clients > 1 {
		t.Logf("\nTest starting with %d parallel clients...", clients)
	}

	startServer(t)

	time.Sleep(1e9)

	//msg := bytes.Repeat([]byte("X"), msg_size)

	for iters := 0; iters < passes; iters++ {
		t.Log("Starting pass", iters)
		clientDisconnect := new(sync.WaitGroup)
		clientDisconnect.Add(clients)

		for i := 0; i < clients; i++ {

			go func() {
				clientMessage := make(chan Message)

				client := NewWebsocketClient(SIOCodec{})
				client.OnMessage(func(msg Message) {
					clientMessage <- msg
				})
				client.OnDisconnect(func() {
					clientDisconnect.Done()
				})

				err := client.Dial("ws://"+serverAddr+"/socket.io/websocket", "http://"+serverAddr+"/")
				if err != nil {
					t.Fatal(err)
				}

				iook := make(chan bool)

				go func() {
					t.Logf("Sending %d messages of size %v bytes...", numMessages, msg_size)
					var err error

					for i := 0; i < numMessages; i++ {
						if err = client.Send(make([]byte, msg_size)); err != nil {
							t.Fatal("Send ERROR:", err)
						}
					}

					iook <- true
				}()

				go func() {
					t.Log("Receiving messages...")
					for i := 0; i < numMessages; i++ {
						<-clientMessage
					}
					iook <- true
				}()

				for i := 0; i < 2; i++ {
					<-iook
				}

				go func() {
					if err = client.Close(); err != nil {
						t.Fatal("Close ERROR:", err)
					}
				}()
			}()
		}

		t.Log("Waiting for clients disconnect")
		clientDisconnect.Wait()

		fmt.Printf("Sent %v messages * %v concurrent clients = %v messages\n", numMessages, clients, numMessages*clients)

		time.Sleep(1e9)
	}

	time.Sleep(10e9)
}
