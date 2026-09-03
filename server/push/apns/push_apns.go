// Package apns implements a push notification adapter that delivers real
// PushKit VoIP pushes to iOS directly via Apple's APNs HTTP/2 API.
//
// This exists because FCM v1 does not support relaying a genuine
// "apns-push-type: voip" push (confirmed via Firebase's own open feature
// request "FCM support for VoIP push", unresolved since 2023) — see the FIXME
// comment in ../fcm/payload.go. A locked or killed iOS app can only be woken
// to show a CallKit incoming-call UI by a real VoIP push, so this adapter
// handles ONLY call-start events for iOS devices that have registered a VoIP
// token (types.DeviceDef.VoipToken). Every other push (regular alerts,
// Android, web, and the FCM fallback alert for call-start too) is untouched
// and keeps going through the existing fcm adapter.
package apns

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/sideshow/apns2"
	"github.com/sideshow/apns2/token"

	"github.com/tinode/chat/server/logs"
	"github.com/tinode/chat/server/push"
	"github.com/tinode/chat/server/store"
	t "github.com/tinode/chat/server/store/types"
)

const bufferSize = 1024

// voipExpirySeconds mirrors fcm's voipTimeToLive: a VoIP push is only useful
// for the few seconds a call is actually ringing.
const voipExpirySeconds = 10

var handler Handler

// Handler represents state of the direct-APNs VoIP push client.
type Handler struct {
	input   chan *push.Receipt
	channel chan *push.ChannelReq
	stop    chan bool
	client  *apns2.Client
	topic   string
}

type configType struct {
	Enabled bool `json:"enabled"`
	// Regular app bundle ID, e.g. "app.chatapp.p2p.tinode". The adapter sends
	// to "<BundleID>.voip" per Apple's VoIP push requirements.
	BundleID string `json:"bundle_id"`
	KeyID    string `json:"key_id"`
	TeamID   string `json:"team_id"`
	// Exactly one of P8Key (raw key contents) or P8KeyFile (path) is required.
	P8Key     string `json:"p8_key"`
	P8KeyFile string `json:"p8_key_file"`
	// Use Apple's sandbox APNs environment instead of production.
	Sandbox bool `json:"sandbox"`
}

// Init initializes the handler.
func (Handler) Init(jsonconf json.RawMessage) (bool, error) {
	var config configType
	if err := json.Unmarshal(jsonconf, &config); err != nil {
		return false, errors.New("apns: failed to parse config: " + err.Error())
	}

	if !config.Enabled {
		return false, nil
	}
	if config.BundleID == "" || config.KeyID == "" || config.TeamID == "" {
		return false, errors.New("apns: bundle_id, key_id and team_id are all required")
	}

	var keyBytes []byte
	var err error
	if config.P8KeyFile != "" {
		if keyBytes, err = os.ReadFile(config.P8KeyFile); err != nil {
			return false, errors.New("apns: failed to read p8_key_file: " + err.Error())
		}
	} else if config.P8Key != "" {
		keyBytes = []byte(config.P8Key)
	} else {
		return false, errors.New("apns: either p8_key or p8_key_file must be set")
	}

	authKey, err := token.AuthKeyFromBytes(keyBytes)
	if err != nil {
		return false, errors.New("apns: invalid p8 auth key: " + err.Error())
	}

	tok := &token.Token{
		AuthKey: authKey,
		KeyID:   config.KeyID,
		TeamID:  config.TeamID,
	}

	client := apns2.NewTokenClient(tok)
	if config.Sandbox {
		client = client.Development()
	} else {
		client = client.Production()
	}

	handler.client = client
	handler.topic = config.BundleID + ".voip"
	handler.input = make(chan *push.Receipt, bufferSize)
	handler.channel = make(chan *push.ChannelReq, bufferSize)
	handler.stop = make(chan bool, 1)

	go func() {
		for {
			select {
			case rcpt := <-handler.input:
				go sendVoipPushes(rcpt)
			case <-handler.channel:
				// This adapter delivers only 1:1 call-start VoIP pushes; it has
				// no concept of FCM-topic-style channel subscriptions.
			case <-handler.stop:
				return
			}
		}
	}()

	return true, nil
}

// sendVoipPushes filters a receipt down to iOS devices with a registered VoIP
// token, but only for a call-start event - everything else is left for the
// fcm adapter to handle as it already does today.
func sendVoipPushes(rcpt *push.Receipt) {
	if rcpt.Payload.Webrtc != "started" {
		return
	}

	uids := make([]t.Uid, 0, len(rcpt.To))
	for uid := range rcpt.To {
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		return
	}

	devices, count, err := store.Devices.GetAll(uids...)
	if err != nil {
		logs.Warn.Println("apns: failed to load devices:", err)
		return
	}
	if count == 0 {
		logs.Info.Println("apns: call-start push, but no devices found for", uids)
		return
	}

	sent := 0
	for uid, devList := range devices {
		for i := range devList {
			d := &devList[i]
			logs.Info.Println("apns: candidate device", d.DeviceId, "platform", d.Platform, "hasVoipToken", d.VoipToken != "")
			if d.Platform == "ios" && d.VoipToken != "" {
				sent++
				sendOne(uid, d, &rcpt.Payload)
			}
		}
	}
	if sent == 0 {
		logs.Info.Println("apns: call-start push, but no eligible iOS+voiptoken device among", count, "device(s)")
	}
}

// sendOne sends a single real VoIP push. The "data" fields mirror exactly
// what Tinodios/AppDelegate.swift's didReceiveIncomingPushWith(...) already
// parses from a VoIP push payload - topic/xfrom/seq as strings, aonly as a
// real JSON bool (this adapter isn't constrained to FCM's string-only data
// values), so the existing client-side parsing doesn't need to change.
func sendOne(uid t.Uid, d *t.DeviceDef, pl *push.Payload) {
	data := map[string]any{
		"topic":  pl.Topic,
		"xfrom":  pl.From,
		"seq":    strconv.Itoa(pl.SeqId),
		"webrtc": pl.Webrtc,
	}
	if pl.AudioOnly {
		data["aonly"] = true
	}

	body, err := json.Marshal(map[string]any{
		"aps":  map[string]any{},
		"data": data,
	})
	if err != nil {
		logs.Warn.Println("apns: failed to marshal voip payload:", err)
		return
	}

	notification := &apns2.Notification{
		DeviceToken: d.VoipToken,
		Topic:       handler.topic,
		PushType:    apns2.PushTypeVOIP,
		Priority:    apns2.PriorityHigh,
		Expiration:  time.Now().Add(voipExpirySeconds * time.Second),
		Payload:     body,
	}

	logs.Info.Println("apns: sending voip push to device", d.DeviceId, "topic", handler.topic)
	res, err := handler.client.Push(notification)
	if err != nil {
		logs.Warn.Println("apns: voip push transport error:", err)
		return
	}
	if res.Sent() {
		logs.Info.Println("apns: voip push sent OK, apns-id", res.ApnsID)
		return
	}

	logs.Warn.Println("apns: voip push rejected:", res.StatusCode, res.Reason)
	switch res.Reason {
	case apns2.ReasonBadDeviceToken, apns2.ReasonUnregistered, apns2.ReasonDeviceTokenNotForTopic:
		// The VoIP token is no longer valid. Clear only it, keep the regular
		// device (FCM) token on the same row intact.
		if err := store.Devices.Update(uid, d.DeviceId, &t.DeviceDef{
			DeviceId: d.DeviceId,
			Platform: d.Platform,
			LastSeen: d.LastSeen,
			Lang:     d.Lang,
			// VoipToken intentionally omitted (zero value) to clear it.
		}); err != nil {
			logs.Warn.Println("apns: failed to clear invalid voip token:", err)
		}
	}
}

// IsReady checks if the handler is initialized.
func (Handler) IsReady() bool {
	return handler.input != nil
}

// Push returns a channel that the server will use to send messages to.
// If the adapter blocks, the message will be dropped.
func (Handler) Push() chan<- *push.Receipt {
	return handler.input
}

// Channel returns a channel for group (FCM-topic-style) requests. Unused by
// this adapter - see the no-op case in Init's select loop.
func (Handler) Channel() chan<- *push.ChannelReq {
	return handler.channel
}

// Stop terminates the handler's worker and stops sending pushes.
func (Handler) Stop() {
	handler.stop <- true
}

func init() {
	push.Register("apns", &handler)
}
