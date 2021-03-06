package main

import (
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/miekg/dns"
	"github.com/semihalev/log"
)

const (
	testNameserver = "127.0.0.1:53"
	testDomain     = "www.google.com"
)

var (
	ginr *gin.Engine
)

func TestMain(m *testing.M) {
	log.Root().SetHandler(log.LvlFilterHandler(0, log.StdoutHandler))

	gin.SetMode(gin.TestMode)
	ginr = gin.New()

	block := ginr.Group("/api/v1/block")
	{
		block.GET("/exists/:key", existsBlock)
		block.GET("/get/:key", getBlock)
		block.GET("/remove/:key", removeBlock)
		block.GET("/set/:key", setBlock)
	}

	m.Run()
}

func Test_SDNS(t *testing.T) {
	Config.Bind = ":0"
	Config.BindTLS = ""
	Config.API = ""

	startSDNS()

	time.Sleep(2 * time.Second)
}

func BenchmarkResolver(b *testing.B) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(testDomain), dns.TypeA)

	c := new(dns.Client)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		c.Exchange(m, testNameserver)
	}
}
