package main

import (
	"fmt"
	socketio "github.com/googollee/go-socket.io"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"html/template"
	"io"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	rtcpPLIInterval = time.Second * 3
)

func checkError(err error) {
	if err != nil {
		panic(err)
	}
}

var (
	requestChan    chan string
	ConnChan       chan socketio.Conn
	server         *socketio.Server
	PublisherCount int32
)

func web(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		t, _ := template.ParseFiles("index.html")
		checkError(t.Execute(w, nil))
	}
}
func init() {
	requestChan = make(chan string, 5)
	ConnChan = make(chan socketio.Conn, 5)
}
func main() {
	t, err := socketio.NewServer(nil)
	if err != nil {
		log.Fatal(err)
	}
	server = t
	requestChan = make(chan string, 2)
	ConnChan = make(chan socketio.Conn, 2)
	server.OnConnect("/", func(s socketio.Conn) error {
		s.SetContext("")
		fmt.Println("connected:", s.ID())
		return nil
	})
	server.OnEvent("/", "sdp", func(s socketio.Conn, msg string) {
		println("data")
		requestChan <- msg
		ConnChan <- s
		println("after")
	})
	go server.Serve()
	go room()
	defer server.Close()
	http.Handle("/socket.io/", server)
	http.Handle("/", http.FileServer(http.Dir("./asset")))
	http.HandleFunc("/index", web)
	log.Println("Serving at localhost:8000...")
	//log.Fatal(http.ListenAndServe(":8000", nil))
	http.ListenAndServeTLS(":8443", "cert.pem", "key.pem", nil)
}

func room() {

	sdp := <-requestChan
	conn := <-ConnChan
	atomic.AddInt32(&PublisherCount, 1)
	offer := webrtc.SessionDescription{}
	Decode(sdp, &offer)
	mediaEngine := webrtc.MediaEngine{}
	err := mediaEngine.PopulateFromSDP(offer)
	if err != nil {
		panic(err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	peerConnectionConfig := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(peerConnectionConfig)
	if err != nil {
		panic(err)
	}

	// Allow us to receive 1 video track
	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	localTrackChan := make(chan *webrtc.Track)
	peerConnection.OnTrack(func(remoteTrack *webrtc.Track, receiver *webrtc.RTPReceiver) {
		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		// This can be less wasteful by processing incoming RTCP events, then we would emit a NACK/PLI when a viewer requests it
		go func() {
			ticker := time.NewTicker(rtcpPLIInterval)
			for range ticker.C {
				if rtcpSendErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: remoteTrack.SSRC()}}); rtcpSendErr != nil {
					fmt.Println(rtcpSendErr)
				}
			}
		}()

		// Create a local track, all our SFU clients will be fed via this track
		localTrack, newTrackErr := peerConnection.NewTrack(remoteTrack.PayloadType(), remoteTrack.SSRC(), "video", "pion")
		if newTrackErr != nil {
			panic(newTrackErr)
		}
		localTrackChan <- localTrack

		rtpBuf := make([]byte, 1400)
		for {
			i, readErr := remoteTrack.Read(rtpBuf)
			if readErr != nil {
				panic(readErr)
			}

			// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
			if _, err = localTrack.Write(rtpBuf[:i]); err != nil && err != io.ErrClosedPipe {
				panic(err)
			}
		}
	})
	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}
	//Encode(*peerConnection.LocalDescription())
	go conn.Emit("sdp", Encode(*peerConnection.LocalDescription()))

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete
	localTrack := <-localTrackChan
	for {
		sdp := <-requestChan
		conn := <-ConnChan

		recvOnlyOffer := webrtc.SessionDescription{}
		Decode(sdp, &recvOnlyOffer)

		// Create a new PeerConnection
		peerConnection, err := api.NewPeerConnection(peerConnectionConfig)
		if err != nil {
			panic(err)
		}

		_, err = peerConnection.AddTrack(localTrack)
		if err != nil {
			panic(err)
		}

		// Set the remote SessionDescription
		err = peerConnection.SetRemoteDescription(recvOnlyOffer)
		if err != nil {
			panic(err)
		}

		// Create answer
		answer, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			panic(err)
		}

		// Create channel that is blocked until ICE Gathering is complete
		gatherComplete = webrtc.GatheringCompletePromise(peerConnection)

		// Sets the LocalDescription, and starts our UDP listeners
		err = peerConnection.SetLocalDescription(answer)
		if err != nil {
			panic(err)
		}

		// Block until ICE Gathering is complete, disabling trickle ICE
		// we do this because we only can exchange one signaling message
		// in a production application you should exchange ICE Candidates via OnICECandidate
		<-gatherComplete

		// Get the LocalDescription and take it to base64 so we can paste in browser
		go conn.Emit("sdp", Encode(*peerConnection.LocalDescription()))
	}

}
