// SPDX-License-Identifier: MIT
//
// Copyright © 2018 Kent Gibson <warthog618@gmail.com>.

// Package gsm provides a driver for GSM modems.
package gsm

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/warthog618/modem/at"
	"github.com/warthog618/modem/info"
	"github.com/warthog618/sms"
	"github.com/warthog618/sms/encoding/pdumode"
	"github.com/warthog618/sms/encoding/tpdu"
)

// GSM modem decorates the AT modem with GSM specific functionality.
type GSM struct {
	*at.AT
	sca     pdumode.SMSCAddress
	pduMode bool
	eOpts   []sms.EncoderOption
}

// Option is a construction option for the GSM.
type Option interface {
	applyOption(*GSM)
}

// New creates a new GSM modem.
func New(a *at.AT, options ...Option) *GSM {
	g := GSM{AT: a, pduMode: true}
	for _, option := range options {
		option.applyOption(&g)
	}
	return &g
}

type encoderOption struct {
	sms.EncoderOption
}

func (o encoderOption) applyOption(g *GSM) {
	g.eOpts = append(g.eOpts, o)
}

// WithEncoderOption applies the encoder option when converting from text
// messages to SMS TPDUs.
//
func WithEncoderOption(eo sms.EncoderOption) Option {
	return encoderOption{eo}
}

type pduModeOption bool

func (o pduModeOption) applyOption(g *GSM) {
	g.pduMode = bool(o)
}

// WithPDUMode specifies that the modem is to be used in PDU mode.
//
// This is the default mode.
var WithPDUMode = pduModeOption(true)

// WithTextMode specifies that the modem is to be used in text mode.
//
// This overrides is the default PDU mode.
var WithTextMode = pduModeOption(false)

type scaOption pdumode.SMSCAddress

// WithSCA sets the SCA used when transmitting SMSs in PDU mode.
//
// This overrides the default set in the SIM.
//
// The SCA is only relevant in PDU mode, so this option also enables PDU mode.
func WithSCA(sca pdumode.SMSCAddress) Option {
	return scaOption(sca)
}

func (o scaOption) applyOption(g *GSM) {
	g.pduMode = true
	g.sca = pdumode.SMSCAddress(o)
}

// Init initialises the GSM modem.
func (g *GSM) Init(options ...at.InitOption) (err error) {
	if err = g.AT.Init(options...); err != nil {
		return
	}
	// test GCAP response to ensure +GSM support, and modem sync.
	var i []string
	i, err = g.Command("+GCAP")
	if err != nil {
		return
	}
	capabilities := make(map[string]bool)
	for _, l := range i {
		if info.HasPrefix(l, "+GCAP") {
			caps := strings.Split(info.TrimPrefix(l, "+GCAP"), ",")
			for _, cap := range caps {
				capabilities[cap] = true
			}
		}
	}
	if !capabilities["+CGSM"] {
		return ErrNotGSMCapable
	}
	cmds := []string{
		"+CMGF=1", // text mode
		"+CMEE=2", // textual errors
	}
	if g.pduMode {
		cmds[0] = "+CMGF=0" // pdu mode
	}
	for _, cmd := range cmds {
		_, err = g.Command(cmd)
		if err != nil {
			return
		}
	}
	return
}

// SendShortMessage sends an SMS message to the number.
//
// If the modem is in PDU mode then the message is converted to a single SMS
// PDU.
//
// The mr is returned on success, else an error.
func (g *GSM) SendShortMessage(number string, message string, options ...at.CommandOption) (rsp string, err error) {
	if g.pduMode {
		var pdus []tpdu.TPDU
		eOpts := append(g.eOpts, sms.To(number))
		pdus, err = sms.Encode([]byte(message), eOpts...)
		if err != nil {
			return
		}
		if len(pdus) > 1 {
			err = ErrOverlength
			return
		}
		var tp []byte
		tp, err = pdus[0].MarshalBinary()
		if err != nil {
			return
		}
		return g.SendPDU(tp, options...)
	}
	var i []string
	i, err = g.SMSCommand("+CMGS=\""+number+"\"", message, options...)
	if err != nil {
		return
	}
	// parse response, ignoring any lines other than well-formed.
	for _, l := range i {
		if info.HasPrefix(l, "+CMGS") {
			rsp = info.TrimPrefix(l, "+CMGS")
			return
		}
	}
	err = ErrMalformedResponse
	return
}

// SendLongMessage sends an SMS message to the number.
//
// The modem must be in PDU mode.
// The message is split into concatenated SMS PDUs, if necessary.
//
// The mr of send PDUs is returned on success, else an error.
func (g *GSM) SendLongMessage(number string, message string, options ...at.CommandOption) (rsp []string, err error) {
	if !g.pduMode {
		err = ErrWrongMode
		return
	}
	var pdus []tpdu.TPDU
	eOpts := append(g.eOpts, sms.To(number))
	pdus, err = sms.Encode([]byte(message), eOpts...)
	if err != nil {
		return
	}
	for _, p := range pdus {
		var tp []byte
		tp, err = p.MarshalBinary()
		if err != nil {
			return
		}
		var mr string
		mr, err = g.SendPDU(tp, options...)
		if len(mr) > 0 {
			rsp = append(rsp, mr)
		}
		if err != nil {
			return
		}
	}
	return
}

// SendPDU sends an SMS PDU.
//
// tpdu is the binary TPDU to be sent.
// The mr is returned on success, else an error.
func (g *GSM) SendPDU(tpdu []byte, options ...at.CommandOption) (rsp string, err error) {
	if !g.pduMode {
		return "", ErrWrongMode
	}
	pdu := pdumode.PDU{SMSC: g.sca, TPDU: tpdu}
	var s string
	s, err = pdu.MarshalHexString()
	if err != nil {
		return
	}
	var i []string
	i, err = g.SMSCommand(fmt.Sprintf("+CMGS=%d", len(tpdu)), s, options...)
	if err != nil {
		return
	}
	// parse response, ignoring any lines other than well-formed.
	for _, l := range i {
		if info.HasPrefix(l, "+CMGS") {
			rsp = info.TrimPrefix(l, "+CMGS")
			return
		}
	}
	err = ErrMalformedResponse
	return
}

// MessageHandler receives a decoded SMS message from the modem.
type MessageHandler func(number string, message string)

// ErrorHandler receives asynchronous errors.
type ErrorHandler func(error)

// StartMessageRx sets up the modem to receive SMS messages and pass them to
// the message handler.
//
// The message may have been concatenated over several SMS PDUs, but if so is
// reassembled into a complete message before being passed to the message
// handler.
//
// Errors detected while receiving messages are passed to the error handler.
//
// Requires the modem to be in PDU mode.
func (g *GSM) StartMessageRx(mh MessageHandler, eh ErrorHandler) error {
	if !g.pduMode {
		return ErrWrongMode
	}
	c := sms.NewCollector()
	cmtHandler := func(info []string) {
		tp, err := UnmarshalTPDU(info)
		if err != nil {
			eh(err)
			return
		}
		g.Command("+CNMA")
		tpdus, err := c.Collect(tp)
		if err != nil {
			eh(err)
			return
		}
		m, err := sms.Decode(tpdus)
		if err != nil {
			eh(err)
		}
		if m != nil {
			mh(tpdus[0].OA.Number(), string(m))
		}
	}
	err := g.AddIndication("+CMT:", cmtHandler, at.WithTrailingLine)
	if err != nil {
		return err
	}
	// tell the modem to forward SMS-DELIVERs via +CMT indications...
	_, err = g.Command("+CNMI=1,2,0,0,0")
	if err != nil {
		g.CancelIndication("+CMT:")
	}
	return err
}

// StopMessageRx ends the reception of messages started by StartMessageRx,
func (g *GSM) StopMessageRx() {
	// tell the modem to stop forwarding SMSs to us.
	g.Command("+CNMI=0,0,0,0,0")
	// and detach the handler
	g.CancelIndication("+CMT:")
}

// UnmarshalTPDU converts +CMT info into the corresponding SMS TPDU.
func UnmarshalTPDU(info []string) (tp tpdu.TPDU, err error) {
	if len(info) < 2 {
		err = ErrUnderlength
		return
	}
	lstr := strings.Split(info[0], ",")
	var l int
	l, err = strconv.Atoi(lstr[len(lstr)-1])
	if err != nil {
		return
	}
	var pdu *pdumode.PDU
	pdu, err = pdumode.UnmarshalHexString(info[1])
	if err != nil {
		return
	}
	if int(l) != len(pdu.TPDU) {
		err = fmt.Errorf("length mismatch - expected %d, got %d", l, len(pdu.TPDU))
		return
	}
	err = tp.UnmarshalBinary(pdu.TPDU)
	return
}

var (
	// ErrMalformedResponse indicates the modem returned a badly formed
	// response.
	ErrMalformedResponse = errors.New("modem returned malformed response")

	// ErrNotGSMCapable indicates that the modem does not support the GSM
	// command set, as determined from the GCAP response.
	ErrNotGSMCapable = errors.New("modem is not GSM capable")

	// ErrNotPINReady indicates the modem SIM card is not ready to perform
	// operations.
	ErrNotPINReady = errors.New("modem is not PIN Ready")

	// ErrOverlength indicates the message is too long for a single PDU and
	// must be split into multiple PDUs.
	ErrOverlength = errors.New("message too long for one SMS")

	// ErrUnderlength indicates that two few lines of info were provided to
	// decode a PDU.
	ErrUnderlength = errors.New("insufficient info")

	// ErrWrongMode indicates the GSM modem is operating in the wrong mode and
	// so cannot support the command.
	ErrWrongMode = errors.New("modem is in the wrong mode")
)
