package runner

import (
	"bufio"
	"context"
	"github.com/google/gopacket/pcap"
	"github.com/phayes/freeport"
	"go.uber.org/ratelimit"
	"ksubdomain/core"
	"ksubdomain/core/device"
	"ksubdomain/core/gologger"
	options2 "ksubdomain/core/options"
	"ksubdomain/runner/statusdb"
	"math/rand"
	"os"
	"strings"
	"time"
)

type runner struct {
	ether           *device.EtherTable //本地网卡信息
	hm              *statusdb.StatusDb
	options         *options2.Options
	limit           ratelimit.Limiter
	handle          *pcap.Handle
	successIndex    uint64
	sendIndex       uint64
	recvIndex       uint64
	faildIndex      uint64
	sender          chan string
	recver          chan core.RecvResult
	freeport        int
	dnsid           uint16 // dnsid 用于接收的确定ID
	maxRetry        int    // 最大重试次数
	timeout         int64  // 超时xx秒后重试
	ctx             context.Context
	fisrtloadChanel chan string // 数据加载完毕的chanel
	startTime       time.Time
}

func GetDeviceConfig() *device.EtherTable {
	filename := "ksubdomain.yaml"
	var ether *device.EtherTable
	var err error
	if core.FileExists(filename) {
		ether, err = device.ReadConfig(filename)
		if err != nil {
			gologger.Fatalf("读取配置失败:%v", err)
		}
		gologger.Infof("读取配置%s成功!\n", filename)
	} else {
		ether = device.AutoGetDevices()
		err = ether.SaveConfig(filename)
		if err != nil {
			gologger.Fatalf("保存配置失败:%v", err)
		}
	}
	gologger.Infof("Use Device: %s\n", ether.Device)
	gologger.Infof("Use IP:%s\n", ether.SrcIp.String())
	gologger.Infof("Local Mac: %s\n", ether.SrcMac.String())
	gologger.Infof("GateWay Mac: %s\n", ether.DstMac.String())
	return ether
}
func New(options *options2.Options) (*runner, error) {
	var err error
	version := pcap.Version()
	r := new(runner)
	r.options = options
	gologger.Infof(version + "\n")
	//if options.ListNetwork {
	//	device.GetIpv4Devices()
	//	os.Exit(0)
	//}
	r.ether = GetDeviceConfig()
	r.hm = statusdb.CreateMemoryDB()

	gologger.Infof("Rate:%dpps\n", options.Rate)
	gologger.Infof("DNS:%s\n", options.Resolvers)
	r.handle, err = device.PcapInit(r.ether.Device)
	if err != nil {
		return nil, err
	}
	r.limit = ratelimit.New(int(options.Rate)) // per second
	r.sender = make(chan string, 999)          // 可多个协程发送
	r.recver = make(chan core.RecvResult)      // 只用一个协程接收，这里不会影响性能

	tmpFreeport, err := freeport.GetFreePort()
	if err != nil {
		return nil, err
	}
	r.freeport = tmpFreeport
	gologger.Infof("FreePort:%d\n", tmpFreeport)
	r.dnsid = 0x2021 // set dnsid 65500
	r.maxRetry = r.options.Retry
	r.timeout = int64(r.options.TimeOut)
	r.ctx = context.Background()
	r.fisrtloadChanel = make(chan string)
	r.startTime = time.Now()

	go r.loadTargets()
	return r, nil
}
func (r *runner) choseDns() string {
	dns := r.options.Resolvers
	return dns[rand.Intn(len(dns))]
}

func (r *runner) loadTargets() {
	// get targets
	var reader *bufio.Reader
	options := r.options
	if options.Method == "verify" {
		if options.Stdin {
			reader = bufio.NewReader(os.Stdin)
		} else {
			f2, err := os.Open(options.FileName)
			if err != nil {
				gologger.Fatalf("打开文件:%s 出现错误:%s", options.FileName, err.Error())
			}
			defer f2.Close()
			reader = bufio.NewReader(f2)
		}
	} else if options.Method == "enum" {
		if options.Stdin {
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Split(bufio.ScanLines)
			for scanner.Scan() {
				options.Domain = append(options.Domain, scanner.Text())
			}
		}
		// 读取字典
		if options.FileName == "" {
			subdomainDict := core.GetDefaultSubdomainData()
			gologger.Infof("加载内置字典:%d\n", len(subdomainDict))
			reader = bufio.NewReader(strings.NewReader(strings.Join(subdomainDict, "\n")))
		} else {
			subdomainDict, err := core.LinesInFile(options.FileName)
			if err != nil {
				gologger.Fatalf("打开文件:%s 错误:%s", options.FileName, err.Error())
			}
			reader = bufio.NewReader(strings.NewReader(strings.Join(subdomainDict, "\n")))
		}
	}
	if options.SkipWildCard && len(options.Domain) > 0 {
		var tmpDomains []string
		gologger.Infof("检测泛解析\n")
		for _, domain := range options.Domain {
			if !core.IsWildCard(domain) {
				tmpDomains = append(tmpDomains, domain)
			} else {
				gologger.Warningf("域名:%s 存在泛解析记录,已跳过\n", domain)
			}
		}
		options.Domain = tmpDomains
	}
	if len(options.Domain) > 0 {
		gologger.Infof("检测域名:%s\n", options.Domain)
	}

	for {
		line, _, err := reader.ReadLine()
		if err != nil {
			break
		}
		msg := string(line)
		if r.options.Method == "verify" {
			// send msg
			r.sender <- msg
		} else {
			for _, tmpDomain := range r.options.Domain {
				newDomain := msg + "." + tmpDomain
				r.sender <- newDomain
			}
		}
	}
	r.fisrtloadChanel <- "ok"
}
func (r *runner) PrintStatus() {
	queue := r.hm.Length()
	tc := int(time.Since(r.startTime).Seconds())
	gologger.Printf("\rSuccess:%d Send:%d Queue:%d Accept:%d Fail:%d Elapsed:%ds", r.successIndex, r.sendIndex, queue, r.recvIndex, r.faildIndex, tc)
}
func (r *runner) RunEnumeration() {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()

	go r.recvChanel(ctx)   // 启动接收线程
	go r.sendCycle(ctx)    // 发送线程
	go r.handleResult(ctx) // 处理结果，打印输出

	var isLoadOver bool = false // 是否加载文件完毕
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.PrintStatus()
			if isLoadOver {
				if r.hm.Length() == 0 {
					gologger.Printf("\n")
					gologger.Infof("扫描完毕")
					return
				}
			}
		case <-r.fisrtloadChanel:
			go r.retry(ctx) // 遍历hm，依次重试
			isLoadOver = true
		}
	}
}

func (r *runner) Close() {
	close(r.recver)
	close(r.sender)
	r.handle.Close()
	r.hm.Close()
}
