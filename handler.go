package main

import (
	"fmt"
	"net"

	"github.com/miekg/dns"
	"github.com/semihalev/log"
)

const (
	// DefaultMsgSize EDNS0 message size
	DefaultMsgSize = 1536
)

// Question type
type Question struct {
	Qname  string `json:"name"`
	Qtype  string `json:"type"`
	Qclass string `json:"class"`
}

// String formats a question
func (q *Question) String() string {
	return fmt.Sprintf("%s %s %s", q.Qname, q.Qclass, q.Qtype)
}

// DNSHandler type
type DNSHandler struct {
	resolver   *Resolver
	cache      *QueryCache
	errorCache *ErrorCache
}

// NewHandler returns a new DNSHandler
func NewHandler() *DNSHandler {
	return &DNSHandler{
		NewResolver(),
		NewQueryCache(Config.Maxcount),
		NewErrorCache(Config.Maxcount, Config.Expire),
	}
}

// TCP begins a tcp query
func (h *DNSHandler) TCP(w dns.ResponseWriter, req *dns.Msg) {
	go h.do("tcp", w, req)
}

// UDP begins a udp query
func (h *DNSHandler) UDP(w dns.ResponseWriter, req *dns.Msg) {
	go h.do("udp", w, req)
}

func (h *DNSHandler) do(proto string, w dns.ResponseWriter, req *dns.Msg) {
	q := req.Question[0]
	Q := Question{unFqdn(q.Name), dns.TypeToString[q.Qtype], dns.ClassToString[q.Qclass]}

	dsReq := false

	opt := req.IsEdns0()
	if opt != nil {
		opt.SetUDPSize(DefaultMsgSize)

		if opt.Version() != 0 {
			opt.SetVersion(0)
			opt.SetExtendedRcode(dns.RcodeBadVers)

			h.handleFailed(w, req, dns.RcodeBadVers, dsReq)
			return
		}

		ops := make([]dns.EDNS0, len(opt.Option))
		copy(ops, opt.Option)
		opt.Option = []dns.EDNS0{}

		for _, option := range ops {
			if option.Option() == dns.EDNS0SUBNET {
				opt.Option = append(opt.Option, option)
			}
		}

		dsReq = opt.Do()

		opt.Header().Ttl = 0
		opt.SetDo()
	} else {
		opt = new(dns.OPT)
		opt.Hdr.Name = "."
		opt.Hdr.Rrtype = dns.TypeOPT
		opt.SetUDPSize(DefaultMsgSize)
		opt.SetDo()

		req.Extra = append(req.Extra, opt)
	}

	if q.Qtype == dns.TypeANY {
		h.handleFailed(w, req, dns.RcodeNotImplemented, dsReq)
		return
	}

	if q.Name != rootzone && req.RecursionDesired == false {
		h.handleFailed(w, req, dns.RcodeServerFailure, dsReq)
		return
	}

	log.Debug("Lookup", "query", Q.String(), "dsreq", dsReq)

	key := keyGen(Q)

	mesg, rl, err := h.cache.Get(key)
	if err == nil {
		log.Debug("Cache hit", "key", key, "query", Q.String())

		if Config.RateLimit > 0 && rl.Limit() {
			log.Warn("Query rate limited", "qname", q.Name, "qtype", dns.TypeToString[q.Qtype])

			h.handleFailed(w, req, dns.RcodeServerFailure, dsReq)
			return
		}

		// we need this copy against concurrent modification of Id
		msg := new(dns.Msg)
		*msg = *mesg

		msg.Id = req.Id
		msg = h.checkGLUE(proto, req, msg)

		if !dsReq {
			msg = clearDNSSEC(msg)
		}

		msg = clearOPT(msg)

		opt.SetDo(dsReq)
		msg.Extra = append(msg.Extra, opt)

		h.writeReplyMsg(w, msg)
		return
	}

	err = h.errorCache.Get(key)
	if err == nil {
		log.Debug("Error cache hit", "key", key, "query", Q.String())

		h.handleFailed(w, req, dns.RcodeServerFailure, dsReq)
		return
	}

	if q.Qtype == dns.TypeA || q.Qtype == dns.TypeAAAA {
		if blockCache.Exists(Q.Qname) {
			m := new(dns.Msg)
			m.SetReply(req)

			nullroute := net.ParseIP(Config.Nullroute)
			nullroutev6 := net.ParseIP(Config.Nullroutev6)

			switch q.Qtype {
			case dns.TypeA:
				rrHeader := dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeA,
					Class:  dns.ClassINET,
					Ttl:    Config.Expire,
				}
				a := &dns.A{Hdr: rrHeader, A: nullroute}
				m.Answer = append(m.Answer, a)
			case dns.TypeAAAA:
				rrHeader := dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeAAAA,
					Class:  dns.ClassINET,
					Ttl:    Config.Expire,
				}
				a := &dns.AAAA{Hdr: rrHeader, AAAA: nullroutev6}
				m.Answer = append(m.Answer, a)
			}

			m.AuthenticatedData = true
			m.Authoritative = false
			m.RecursionAvailable = true

			h.writeReplyMsg(w, m)

			log.Debug("Found in blocklist", "name", Q.Qname)

			err := h.cache.Set(key, m)
			if err != nil {
				log.Error("Set block cache failed", "query", Q.String(), "error", err.Error())
			}

			return
		}
	}

	depth := Config.Maxdepth
	mesg, err = h.resolver.Resolve(proto, req, rootservers, true, depth, 0, false, nil)
	if err != nil {
		log.Warn("Resolve query failed", "query", Q.String(), "error", err.Error())

		h.errorCache.Set(key)

		h.handleFailed(w, req, dns.RcodeServerFailure, dsReq)
		return
	}

	if mesg.Truncated && proto == "udp" {
		h.writeReplyMsg(w, mesg)
		return
	}

	if mesg.Rcode == dns.RcodeSuccess &&
		len(mesg.Answer) == 0 && len(mesg.Ns) == 0 {

		h.errorCache.Set(key)

		h.handleFailed(w, req, dns.RcodeServerFailure, dsReq)
		return
	}

	msg := new(dns.Msg)
	*msg = *mesg

	msg = h.checkGLUE(proto, req, msg)

	if !dsReq {
		msg = clearDNSSEC(msg)
	}

	msg = clearOPT(msg)

	opt.SetDo(dsReq)
	msg.Extra = append(msg.Extra, opt)

	h.writeReplyMsg(w, msg)

	err = h.cache.Set(key, mesg)
	if err != nil {
		log.Error("Set msg failed", "query", Q.String(), "error", err.Error())
		return
	}

	log.Debug("Set msg into cache", "query", Q.String())
}

func (h *DNSHandler) checkGLUE(proto string, req, mesg *dns.Msg) *dns.Msg {
	//check cname response
	answerFound := false
	var cnameReq dns.Msg

	cnameReq.SetEdns0(DefaultMsgSize, true)

	for _, answer := range mesg.Answer {
		if answer.Header().Rrtype == req.Question[0].Qtype {
			answerFound = true
		}

		if answer.Header().Rrtype == dns.TypeCNAME {
			cnameAnswer, _ := answer.(*dns.CNAME)
			cnameReq.SetQuestion(cnameAnswer.Target, req.Question[0].Qtype)
		}
	}

	cnameDepth := 5

	if !answerFound && len(cnameReq.Question) > 0 {
	lookup:
		q := cnameReq.Question[0]
		Q := Question{unFqdn(q.Name), dns.TypeToString[q.Qtype], dns.ClassToString[q.Qclass]}
		childCNAME := false

		log.Debug("Lookup", "query", Q.String())

		key := keyGen(Q)
		respCname, _, err := h.cache.Get(key)
		if err == nil {
			log.Debug("Cache hit", "key", key, "query", Q.String())
			for _, answerCname := range respCname.Answer {
				mesg.Answer = append(mesg.Answer, answerCname)
				if answerCname.Header().Rrtype == dns.TypeCNAME {
					cnameAnswer, _ := answerCname.(*dns.CNAME)
					cnameReq.Question[0].Name = cnameAnswer.Target
					childCNAME = true
				}
			}
		} else {
			depth := Config.Maxdepth
			respCname, err := h.resolver.Resolve(proto, &cnameReq, rootservers, true, depth, 0, false, nil)
			if err == nil && len(respCname.Answer) > 0 && respCname.Rcode == dns.RcodeSuccess {
				for _, answerCname := range respCname.Answer {
					mesg.Answer = append(mesg.Answer, answerCname)
					if answerCname.Header().Rrtype == dns.TypeCNAME {
						cnameAnswer, _ := answerCname.(*dns.CNAME)
						cnameReq.Question[0].Name = cnameAnswer.Target
						childCNAME = true
					}
				}

				err = h.cache.Set(key, respCname)
				if err != nil {
					log.Error("Set query cache failed", "query", Q.String(), "error", err.Error())
				} else {
					log.Debug("Set query into cache", "query", Q.String())
				}
			}
		}

		cnameDepth--

		if childCNAME && cnameDepth > 0 {
			goto lookup
		}
	}

	return mesg
}

func (h *DNSHandler) handleFailed(w dns.ResponseWriter, msg *dns.Msg, rcode int, dsf bool) {
	m := new(dns.Msg)
	m.Extra = msg.Extra
	m.SetRcode(msg, rcode)
	m.RecursionAvailable = true

	if opt := m.IsEdns0(); opt != nil {
		opt.SetDo(dsf)
	}

	h.writeReplyMsg(w, m)
}

func (h *DNSHandler) writeReplyMsg(w dns.ResponseWriter, msg *dns.Msg) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("Recovered in WriteReplyMsg", "recover", r)
		}
	}()

	err := w.WriteMsg(msg)
	if err != nil {
		log.Error("Message writing failed", "error", err.Error())
	}
}
