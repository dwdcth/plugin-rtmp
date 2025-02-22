package rtmp

import (
	"crypto/tls"
	"errors"
	"net"
	"net/url"
	"strings"

	"go.uber.org/zap"
	"m7s.live/engine/v4"
)

func NewRTMPClient(addr string) (client *NetConnection, err error) {
	u, err := url.Parse(addr)
	if err != nil {
		RTMPPlugin.Error("connect url parse", zap.Error(err))
		return nil, err
	}
	ps := strings.Split(u.Path, "/")
	if len(ps) < 3 {
		RTMPPlugin.Error("illegal rtmp url", zap.String("url", addr))
		return nil, errors.New("illegal rtmp url")
	}
	isRtmps := u.Scheme == "rtmps"
	if strings.Count(u.Host, ":") == 0 {
		if isRtmps {
			u.Host += ":443"
		} else {
			u.Host += ":1935"
		}
	}
	var conn net.Conn
	if isRtmps {
		var tlsconn *tls.Conn
		tlsconn, err = tls.Dial("tcp", u.Host, &tls.Config{})
		conn = tlsconn
	} else {
		conn, err = net.Dial("tcp", u.Host)
	}
	if err != nil {
		RTMPPlugin.Error("dial tcp", zap.String("host", u.Host), zap.Error(err))
		return nil, err
	}
	defer func() {
		if err != nil || client == nil {
			conn.Close()
		}
	}()
	client = NewNetConnection(conn)
	err = client.ClientHandshake()
	if err != nil {
		RTMPPlugin.Error("handshake", zap.Error(err))
		return nil, err
	}
	client.appName = ps[1]
	err = client.SendMessage(RTMP_MSG_CHUNK_SIZE, Uint32Message(conf.ChunkSize))
	if err != nil {
		return
	}
	client.writeChunkSize = conf.ChunkSize
	path := u.Path
	if len(u.Query()) != 0 {
		path += "?" + u.RawQuery
	}
	err = client.SendMessage(RTMP_MSG_AMF0_COMMAND, &CallMessage{
		CommandMessage{"connect", 1},
		map[string]any{
			"app":      client.appName,
			"flashVer": "monibuca/" + engine.Engine.Version,
			"swfUrl":   addr,
			"tcUrl":    strings.TrimSuffix(addr, path) + "/" + client.appName,
		},
		nil,
	})
	if err != nil {
		return
	}
	for {
		msg, err := client.RecvMessage()
		if err != nil {
			return nil, err
		}
		switch msg.MessageTypeID {
		case RTMP_MSG_AMF0_COMMAND:
			cmd := msg.MsgData.(Commander).GetCommand()
			switch cmd.CommandName {
			case "_result":
				response := msg.MsgData.(*ResponseMessage)
				if response.Infomation["code"] == NetConnection_Connect_Success {
					return client, nil
				} else {
					return nil, err
				}
			}
		}
	}
}

type RTMPPusher struct {
	RTMPSender
	engine.Pusher
}

func (pusher *RTMPPusher) Connect() (err error) {
	if pusher.NetConnection, err = NewRTMPClient(pusher.RemoteURL); err == nil {
		pusher.SetIO(pusher.NetConnection.Conn)
		RTMPPlugin.Info("connect", zap.String("remoteURL", pusher.RemoteURL))
	}
	return
}

func (pusher *RTMPPusher) Push() error {
	pusher.SendMessage(RTMP_MSG_AMF0_COMMAND, &CommandMessage{"createStream", 2})
	defer pusher.Stop()
	for {
		msg, err := pusher.RecvMessage()
		if err != nil {
			return err
		}
		switch msg.MessageTypeID {
		case RTMP_MSG_AMF0_COMMAND:
			cmd := msg.MsgData.(Commander).GetCommand()
			switch cmd.CommandName {
			case Response_Result, Response_OnStatus:
				if response, ok := msg.MsgData.(*ResponseCreateStreamMessage); ok {
					pusher.StreamID = response.StreamId
					pusher.audio.MessageStreamID = pusher.StreamID
					pusher.video.MessageStreamID = pusher.StreamID
					URL, _ := url.Parse(pusher.RemoteURL)
					_, streamPath, _ := strings.Cut(URL.Path, "/")
					_, streamPath, _ = strings.Cut(streamPath, "/")
					pusher.Args = URL.Query()
					if len(pusher.Args) > 0 {
						streamPath += "?" + pusher.Args.Encode()
					}
					pusher.SendMessage(RTMP_MSG_AMF0_COMMAND, &PublishMessage{
						CURDStreamMessage{
							CommandMessage{
								"publish",
								1,
							},
							response.StreamId,
						},
						streamPath,
						"live",
					})
				} else if response, ok := msg.MsgData.(*ResponsePublishMessage); ok {
					if response.Infomation["code"] == NetStream_Publish_Start {
						go pusher.PlayRaw()
					} else {
						return errors.New(response.Infomation["code"].(string))
					}
				}
			}
		}
	}
}

type RTMPPuller struct {
	RTMPReceiver
	engine.Puller
}

func (puller *RTMPPuller) Connect() (err error) {
	if puller.NetConnection, err = NewRTMPClient(puller.RemoteURL); err == nil {
		puller.SetIO(puller.NetConnection.Conn)
		RTMPPlugin.Info("connect", zap.String("remoteURL", puller.RemoteURL))
	}
	return
}

func (puller *RTMPPuller) Pull() (err error) {
	defer puller.Stop()
	err = puller.SendMessage(RTMP_MSG_AMF0_COMMAND, &CommandMessage{"createStream", 2})
	for err == nil {
		msg, err := puller.RecvMessage()
		if err != nil {
			return err
		}
		switch msg.MessageTypeID {
		case RTMP_MSG_AUDIO:
			puller.ReceiveAudio(msg)
		case RTMP_MSG_VIDEO:
			puller.ReceiveVideo(msg)
		case RTMP_MSG_AMF0_COMMAND:
			cmd := msg.MsgData.(Commander).GetCommand()
			switch cmd.CommandName {
			case "_result":
				if response, ok := msg.MsgData.(*ResponseCreateStreamMessage); ok {
					puller.StreamID = response.StreamId
					m := &PlayMessage{}
					m.StreamId = response.StreamId
					m.TransactionId = 1
					m.CommandMessage.CommandName = "play"
					URL, _ := url.Parse(puller.RemoteURL)
					ps := strings.Split(URL.Path, "/")
					puller.Args = URL.Query()
					m.StreamName = ps[len(ps)-1]
					if len(puller.Args) > 0 {
						m.StreamName += "?" + puller.Args.Encode()
					}
					puller.SendMessage(RTMP_MSG_AMF0_COMMAND, m)
					// if response, ok := msg.MsgData.(*ResponsePlayMessage); ok {
					// 	if response.Object["code"] == "NetStream.Play.Start" {

					// 	} else if response.Object["level"] == Level_Error {
					// 		return errors.New(response.Object["code"].(string))
					// 	}
					// } else {
					// 	return errors.New("pull faild")
					// }
				}
			}
		}
	}
	return
}
