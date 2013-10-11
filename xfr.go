// Copyright 2011 Miek Gieben. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dns

import (
	"net"
	"time"
)

// Envelope is used when doing a transfer with a remote server.
type Envelope struct {
	RR    []RR  // The set of RRs in the answer section of the AXFR reply message.
	Error error // If something went wrong, this contains the error.
}

type Transfer struct {
	Conn
	DialTimeout  time.Duration     // net.DialTimeout (ns), defaults to 2 * 1e9
	ReadTimeout  time.Duration     // net.Conn.SetReadTimeout value for connections (ns), defaults to 2 * 1e9
	WriteTimeout time.Duration     // net.Conn.SetWriteTimeout value for connections (ns), defaults to 2 * 1e9
	tsigTimersOnly   bool
}

// In performs a [AI]XFR request (depends on the message's Qtype). It returns
// a channel of *Envelope on which the replies from the server are sent.
// At the end of the transfer the channel is closed.
// The messages are TSIG checked if needed, no other post-processing is performed.
// The caller must dissect the returned messages.
//
// Basic use pattern for receiving an AXFR:
//
//	// m contains the AXFR request
//	t := new(dns.Transfer)
//	c, e := t.In(m, "127.0.0.1:53")
//	for env := range c
//		// ... deal with env.RR or env.Error
//	}

func (t *Transfer) In(q *Msg, a string, env chan *Envelope) (err error) {
	co := new(Conn)
	timeout := dnsTimeout
	if t.DialTimeout != 0 {
		timeout = t.DialTimeout
	}
	co.Conn, err = net.DialTimeout("tcp", a, timeout)
	if err != nil {
		return err
	}
	if q.Question[0].Qtype == TypeAXFR {
		go t.InAxfr(q.Id, env)
		return nil
	}
	if q.Question[0].Qtype == TypeIXFR {
		go t.InAxfr(q.Id, env)
		return nil
	}
	return nil // TODO(miek): some error
}

func (t *Transfer) InAxfr(id uint16, c chan *Envelope) {
	first := true
	defer t.Close()
	defer close(c)
	for {
		in, err := t.ReadMsg()
		if err != nil {
			c <- &Envelope{nil, err}
			return
		}
		if id != in.Id {
			c <- &Envelope{in.Answer, ErrId}
			return
		}
		if first {
			if !isSOAFirst(in) {
				c <- &Envelope{in.Answer, ErrSoa}
				return
			}
			first = !first
			// only one answer that is SOA, receive more
			if len(in.Answer) == 1 {
				t.tsigTimersOnly = true
				c <- &Envelope{in.Answer, nil}
				continue
			}
		}

		if !first {
			t.tsigTimersOnly = true // Subsequent envelopes use this.
			if isSOALast(in) {
				c <- &Envelope{in.Answer, nil}
				return
			}
			c <- &Envelope{in.Answer, nil}
		}
	}
	panic("dns: not reached")
}

/*
	// re-read 'n stuff must be pushed down
	timeout = dnsTimeout
	if t.ReadTimeout != 0 {
		timeout = t.ReadTimeout
	}
	co.SetReadDeadline(time.Now().Add(dnsTimeout))
	timeout = dnsTimeout
	if t.WriteTimeout != 0 {
		timeout = t.WriteTimeout
	}
	co.SetWriteDeadline(time.Now().Add(dnsTimeout))
	defer co.Close()
	return nil
*/

func (t *Transfer) Out(w ResponseWriter, q *Msg, a string) (chan *Envelope, error) {
	ch := make(chan *Envelope)
	r := new(Msg)
	r.SetReply(q)
	r.Authoritative = true
	go func() {
	for x := range ch {
		// assume it fits TODO(miek): fix
		r.Answer = append(r.Answer, x.RR...)
		if err := w.WriteMsg(r); err != nil {
			return
		}
	}
//		w.TsigTimersOnly(true)
//		rep.Answer = nil
	}()
	return ch, nil
}

// ReadMsg reads a message from the transfer connection t.
func (t *Transfer) ReadMsg() (*Msg, error) {
	m := new(Msg)
	p := make([]byte, MaxMsgSize)
	n, err := t.Read(p)
	if err != nil && n == 0 {
		return nil, err
	}
	p = p[:n]
	if err := m.Unpack(p); err != nil {
		return nil, err
	}
	if ts := m.IsTsig(); t != nil {
		if _, ok := t.TsigSecret[ts.Hdr.Name]; !ok {
			return m, ErrSecret
		}
		// Need to work on the original message p, as that was used to calculate the tsig.
		err = TsigVerify(p, t.TsigSecret[ts.Hdr.Name], t.tsigRequestMAC, t.tsigTimersOnly)
	}
	return m, err
}

// WriteMsg write a message throught the transfer connection t.
func (t *Transfer) WriteMsg(m *Msg) (err error) {
	var out []byte
	if ts := m.IsTsig(); t != nil {
		if _, ok := t.TsigSecret[ts.Hdr.Name]; !ok {
			return ErrSecret
		}
		out, t.tsigRequestMAC, err = TsigGenerate(m, t.TsigSecret[ts.Hdr.Name], t.tsigRequestMAC, t.tsigTimersOnly)
	} else {
		out, err = m.Pack()
	}
	if err != nil {
		return err
	}
	if _, err = t.Write(out); err != nil {
		return err
	}
	return nil
}

/*

func (w *reply) ixfrIn(q *Msg, c chan *Envelope) {
	var serial uint32 // The first serial seen is the current server serial
	first := true
	defer w.conn.Close()
	defer close(c)
	for {
		in, err := w.receive()
		if err != nil {
			c <- &Envelope{in.Answer, err}
			return
		}
		if q.Id != in.Id {
			c <- &Envelope{in.Answer, ErrId}
			return
		}
		if first {
			// A single SOA RR signals "no changes"
			if len(in.Answer) == 1 && checkSOA(in, true) {
				c <- &Envelope{in.Answer, nil}
				return
			}

			// Check if the returned answer is ok
			if !checkSOA(in, true) {
				c <- &Envelope{in.Answer, ErrSoa}
				return
			}
			// This serial is important
			serial = in.Answer[0].(*SOA).Serial
			first = !first
		}

		// Now we need to check each message for SOA records, to see what we need to do
		if !first {
			w.tsigTimersOnly = true
			// If the last record in the IXFR contains the servers' SOA,  we should quit
			if v, ok := in.Answer[len(in.Answer)-1].(*SOA); ok {
				if v.Serial == serial {
					c <- &Envelope{in.Answer, nil}
					return
				}
			}
			c <- &Envelope{in.Answer, nil}
		}
	}
	panic("dns: not reached")
}
*/

func isSOAFirst(in *Msg) bool {
	if len(in.Answer) > 0 {
		return in.Answer[0].Header().Rrtype == TypeSOA
	}
	return false
}

func isSOALast(in *Msg) bool {
	if len(in.Answer) > 0 {
		return in.Answer[len(in.Answer)-1].Header().Rrtype == TypeSOA
	}
	return false
}
