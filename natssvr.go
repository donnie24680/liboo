package oo

import (
	"errors"
	"fmt"
	"github.com/json-iterator/go"
	"github.com/nats-io/go-nats"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//返回的err仅用于日志，若要回发消息，必须明确返回消息部分
type NatsHandler = func(*NatsService, string, *RpcMsg) (*RpcMsg, error)

//也可专用seq来标识chan, 这样subj不再需要chan化
type natsMsg struct {
	subj string
	// rpcmsg *RpcMsg
	data []byte
}

type NatsService struct {
	conn     *nats.Conn
	services *map[string]bool
	ch       *Channel       //管理关闭、发送的ch
	ch_recv  chan *nats.Msg //用于接收
	svrname  string
	svrmark  string
	sndmap   sync.Map //从seq到ch映射
	sndid    uint64   //内部使用的发送序号计数器,每次+1使用
	co_sche  bool     //是否协程方式处理新请求, 默认它就是true

	// msgHandlers *CtxHandlers //消息处理器
	handleMap      sync.Map        //消息处理表，从method到处理函数的回调
	defHandler     NatsHandler     //默认事件处理器
	rspChanHandler NatsHandler     //回复channel消息的处理器
	eventMap       sync.Map        //发给本模块的事件处理表
	chmgr          *ChannelManager //ch管理器

	ctxHandleMap  sync.Map
	ctxEventMap   sync.Map
	defCtxHandler ReqCtxHandler
}

const nats_low_watermark = uint64(1 << 62)

var GNatsService *NatsService

type NatsCfg struct {
	Servers  []string `toml:"servers,omitzero"`
	Mservers []string `toml:"mservers,omitzero"`
	User     string   `toml:"user,omitzero"`
	Pass     string   `toml:"pass,omitzero"`
	Token    string   `toml:"token,omitzero"`
}

func (s *NatsService) WaitForReady() {
	for {
		if nil != s.services {
			return
		}
		time.Sleep(time.Millisecond * 100)
	}

	return
}

func (s *NatsService) fetchAllMonitor(mservers []string) error {
	var ss []string

	type natsConnect struct {
		SubList []string `json:"subscriptions_list,omitempty"`
	}
	type monitorInfo struct {
		Conns []*natsConnect `json:"connections,omitempty"`
	}
	type routesInfo struct {
		Routes []*natsConnect `json:"routes,omitempty"`
	}

	var minfos []*monitorInfo
	var routes []*routesInfo
	for _, url := range mservers {
		connz_url := strings.Trim(url, " ") + "/connz?subs=1"
		connz_rsp, err := http.Get(connz_url)
		if err != nil {
			continue
		}
		minfo := &monitorInfo{}
		if err = jsoniter.NewDecoder(connz_rsp.Body).Decode(minfo); err == nil {
			minfos = append(minfos, minfo)
		}
		connz_rsp.Body.Close()

		routez_url := strings.Trim(url, " ") + "/routez?subs=1"
		routez_rsp, err := http.Get(routez_url)
		if err != nil {
			continue
		}
		route := &routesInfo{}
		if err = jsoniter.NewDecoder(routez_rsp.Body).Decode(route); err == nil {
			routes = append(routes, route)
		}
		routez_rsp.Body.Close()
	}

	for _, minfo := range minfos {
		for _, conn := range minfo.Conns {
			for _, sub := range conn.SubList {
				if strings.Index(sub, "_") == 0 {
					continue
				}
				ss = append(ss, sub)
			}
		}
	}

	for _, route := range routes {
		for _, val := range route.Routes {
			for _, v := range val.SubList {
				if strings.Index(v, "_") == 0 {
					continue
				}
				ss = append(ss, v)
			}
		}
	}

	smap := MakeSubjs(ss)
	s.services = &smap

	return nil
}

// func (s *NatsService) natsSubSpec(nh func(msg *nats.Msg)) {
// 	var subj string

// 	subj = fmt.Sprintf("req.%s.*", s.svrname) //给这类svr的一般消息
// 	s.conn.QueueSubscribe(subj, s.svrname, nh)

// 	subj = fmt.Sprintf("req.%s.*.*.*", s.svrname) //要求回复的给这类Svr的消息
// 	s.conn.QueueSubscribe(subj, s.svrname, nh)

// 	subj = fmt.Sprintf("req.%s.*", s.svrmark) //给本svr的消息
// 	s.conn.QueueSubscribe(subj, s.svrname, nh)

// 	subj = fmt.Sprintf("req.%s.*.*.*", s.svrmark) //给本svr的消息，要求回复
// 	s.conn.QueueSubscribe(subj, s.svrname, nh)

// 	//队列用svrname，以便分类统计消息数，其实因为主题前缀已经限制了svrmark，所以不会争
// 	subj = fmt.Sprintf("rsp.%s.*", s.svrmark) //回复之前的请求
// 	s.conn.QueueSubscribe(subj, s.svrname, nh)

// 	s.conn.QueueSubscribeSyncWithChan(subj, queue, ch)
// 	s.conn.Flush()
// }
func (s *NatsService) natsSubSpec() {
	var subj string

	subj = fmt.Sprintf("req.%s.*", s.svrname) //给这类svr的一般消息
	s.conn.QueueSubscribeSyncWithChan(subj, s.svrname, s.ch_recv)

	subj = fmt.Sprintf("req.%s.*.*.*", s.svrname) //要求回复的给这类Svr的消息
	s.conn.QueueSubscribeSyncWithChan(subj, s.svrname, s.ch_recv)

	subj = fmt.Sprintf("req.%s.*", s.svrmark) //给本svr的消息
	s.conn.QueueSubscribeSyncWithChan(subj, s.svrname, s.ch_recv)

	subj = fmt.Sprintf("req.%s.*.*.*", s.svrmark) //给本svr的消息，要求回复
	s.conn.QueueSubscribeSyncWithChan(subj, s.svrname, s.ch_recv)

	//队列用svrname，以便分类统计消息数，其实因为主题前缀已经限制了svrmark，所以不会争
	subj = fmt.Sprintf("rsp.%s.*", s.svrmark) //回复之前的请求
	s.conn.QueueSubscribeSyncWithChan(subj, s.svrname, s.ch_recv)

	s.conn.Flush()
}

func (s *NatsService) natsHandler(msg *nats.Msg) {
	defer func() {
		if errs := recover(); errs != nil {
			LogW("recover natsHandler.err=%v", errs)
		}
	}()
	// LogD("Get one nats :%s", msg.Subject)
	subjs := strings.Split(msg.Subject, ".") //形如req.*.*[.*.*]/rsp.*.*/evt.*.*
	if len(subjs) != 3 && len(subjs) != 5 {
		LogD("error nats cmd format %s, %d", subjs, len(subjs))
		return
	}

	rpcmsg := &RpcMsg{}
	if err := jsoniter.Unmarshal(msg.Data, rpcmsg); err != nil { //格式错了
		LogD("Failed to Unmarshal %s, err=%v", string(msg.Data), err)
		return
	}

	switch subjs[0] {
	//先看是否回复
	case "rsp":
		cid, err := strconv.ParseUint(subjs[2], 10, 64)
		if err != nil {
			return
		}
		if cid > nats_low_watermark { //这种是请求等回复
			if ch, ok := s.sndmap.Load(cid); ok {
				ch.(chan *RpcMsg) <- rpcmsg //有可能异常
			}
			return
		}
		//非等待形式的
		if s.rspChanHandler != nil {
			if s.co_sche {
				go s.rspChanHandler(s, subjs[2], rpcmsg)
			} else {
				s.rspChanHandler(s, subjs[2], rpcmsg)
			}
		}

		//再看是否注册, 注意    **不是按主题分发**    **而是按消息里的cmd分发**
	case "req":

		fn := s.defCtxHandler
		if v, ok := s.ctxHandleMap.Load(rpcmsg.Cmd); ok {
			// LogD("Get push hand fn, %s", rpcmsg.Cmd)
			fn, _ = v.(ReqCtxHandler)
		}

		modcmd := fmt.Sprintf("%s.%s", subjs[1], subjs[2])
		if fn == nil {
			LogD("skip %s: %s", modcmd, rpcmsg.Cmd)
			return
		}

		//调用处理函数
		call_fn := func() {
			defer func() {
				if errs := recover(); errs != nil {
					LogW("cmd:%s.err=%v", modcmd, errs)
				}
			}()
			ctx := MakeReqCtx(s, modcmd, rpcmsg.Sess) //主题放在所构造的ctx里的cmd
			if len(subjs) == 5 {
				ctx.ReplySubject = fmt.Sprintf("rsp.%s.%s", subjs[3], subjs[4])
			}
			retmsg, err := fn(ctx, rpcmsg)
			if err != nil {
				LogD("Failed to process cmd %s: %s, err: %v", modcmd, rpcmsg.Cmd, err)
				// return
			}

			if retmsg != nil && len(subjs) == 5 {
				// reply_subj := fmt.Sprintf("rsp.%s.%s", subjs[3], subjs[4])
				if err := s.PubSubjMsg(ctx.ReplySubject, retmsg); err != nil {
					LogW("Failed to pub natsmsg err:%v", err)
				}
			}
		}
		if s.co_sche {
			//注意：对顺序消息都有可能错序调用
			go call_fn()
		} else {
			call_fn()
		}

	//event必须是有订阅
	case "evt":
		modcmd := fmt.Sprintf("%s.%s", subjs[1], subjs[2])
		if v, ok := s.ctxEventMap.Load(modcmd); ok {
			fn, _ := v.(ReqCtxHandler)
			call_fn := func() {
				ctx := MakeReqCtx(s, modcmd, rpcmsg.Sess)
				fn(ctx, rpcmsg)
			}
			if s.co_sche {
				go call_fn()
			} else {
				call_fn()
			}
		}
	}
}

//检查有没人订阅了这个消息
func (s *NatsService) checkServiceSubscriptions(subj string) bool {
	if s.services == nil {
		return false
	}
	svx := *s.services
	return CheckSubj(svx, subj)
}
func (s *NatsService) PrintServices() {
	if s.services == nil {
		LogD("no services")
	}
	svx := *s.services
	LogD("svx: %v", svx)
}

//若有mservers，则会监控再决定发出
func InitNatsService(mservers []string, servers []string, svrname string) (*NatsService, error) {
	return InitNatsService2(&NatsCfg{
		Mservers: mservers,
		Servers:  servers,
	}, svrname)
}
func InitNatsService2(cfg *NatsCfg, svrname string) (*NatsService, error) {
	opts := nats.GetDefaultOptions()
	opts.Servers = cfg.Servers
	opts.Token = cfg.Token
	opts.User = cfg.User
	opts.Password = cfg.Pass
	opts.Name = fmt.Sprintf("gosvr.%s", GetSvrmark(svrname))
	opts.MaxReconnect = -1
	opts.ReconnectWait = 1 * time.Second //100 * time.Millisecond
	opts.Timeout = 1 * time.Second
	opts.ReconnectBufSize = 64 * 1024 * 1024 //8M

	conn, err := opts.Connect()
	if err != nil {
		return nil, err
	}

	service := &NatsService{conn: conn, svrname: svrname}
	service.chmgr = NewChannelManager(10240) //全局共享：推送入nats的缓冲
	service.ch = service.chmgr.NewChannel()
	service.sndid = nats_low_watermark //请求应答类的值比较大。避开channel专用
	service.svrmark = GetSvrmark(svrname)
	service.co_sche = true                        //默认使用协程，否则无法支持在req里调用请求等回复
	service.ch_recv = make(chan *nats.Msg, 10240) //接收缓冲

	if GNatsService == nil {
		GNatsService = service
	}

	//wait reply
	// service.natsSubSpec(func(msg *nats.Msg) {
	// 	//service会不会被gc?
	// 	service.natsHandler(msg)
	// })
	service.natsSubSpec()

	//monitor
	if len(cfg.Mservers) > 0 {
		go func(service *NatsService, mservers []string) {
			for {
				service.fetchAllMonitor(mservers)

				select {
				case <-service.ch.IsClosed():
					return
				case <-time.After(1 * time.Second):
				}
			}
		}(service, cfg.Mservers)
	}

	return service, nil
}

//开始发送，会堵塞
func (s *NatsService) StartService() {
	// For:
	for {
		select {
		case m, ok := <-s.ch.RecvChan():
			if !ok {
				LogD("nats Closed.")
				return
			}
			nmsg, _ := m.(*natsMsg)
			if err := s.conn.Publish(nmsg.subj, nmsg.data); err != nil {
				LogW("write natscmd %s err: %v", nmsg.subj, err)
				//直接丢弃，不要再回放？
			}
		case nmsg, ok := <-s.ch_recv:
			if !ok {
				LogD("nats recv Closed.")
				return
			}
			s.natsHandler(nmsg)
		}

	}
}

func (s *NatsService) Close() {
	s.conn.Close()
	s.ch.Close()
	close(s.ch_recv)
}

func (s *NatsService) CloseCoroutineFlag() {
	s.co_sche = false
}

//给channel回复的消息，将回调所设定的函数; //这里不要Ctx化
func (s *NatsService) SubRspChanHandle(nh NatsHandler) {
	s.rspChanHandler = nh
}

func compatibleNatsCtxHandler(ctx *ReqCtx, reqmsg *RpcMsg) (rspmsg *RpcMsg, err error) {
	c, _ := ctx.Ctx.(*NatsService)
	natscmd := ctx.Cmd
	if n := strings.LastIndex(natscmd, "."); n > 0 {
		natscmd = string([]byte(natscmd)[n+1:])
	}
	fn := c.defHandler
	if v, ok := c.handleMap.Load(reqmsg.Cmd); ok {
		fn, _ = v.(NatsHandler)
	}

	if fn != nil {
		rspmsg, err = fn(c, natscmd, reqmsg)
	}
	return
}

//收到req.svrname.XXX[.*.*]时，将回调所设定的函数。若原消息要求回复，此函数的返回值就是回复
//这里的natscmd，是指rpcmsg.Cmd，而不是指nats主题！XXX的值不影响本地分发
//可以订阅别个模块的rpcmsg.Cmd消息，但前提是收得到才能进行处理。
func (s *NatsService) SubMsgHandle(natscmd string, nh interface{}) {
	if n := strings.LastIndex(natscmd, "."); n < 0 {
		// natscmd = string([]byte(natscmd)[n+1:])
		// bank注册没写模块名，统一处理成带模块名的，以便回调时查找.
		natscmd = fmt.Sprintf("%s.%s", s.svrname, natscmd)
	}
	if nh == nil {
		s.handleMap.Delete(natscmd)
		s.ctxHandleMap.Delete(natscmd)
		return
	}
	switch nh.(type) {
	case ReqCtxHandler:
		s.ctxHandleMap.Store(natscmd, nh)
	case NatsHandler:
		s.handleMap.Store(natscmd, nh)
		s.ctxHandleMap.Store(natscmd, compatibleNatsCtxHandler)
	default:
		panic("handle func type error")
	}
}
func (s *NatsService) SubHandleMap(msgMap map[string]ReqCtxHandler, evtMap map[string]ReqCtxHandler) {
	for cmd, nh := range msgMap {
		s.SubMsgHandle(cmd, nh)
	}
	for cmd, nh := range evtMap {
		s.SubEventHandle(cmd, nh)
	}
}

//所有未注册req处理器的消息，将回调所设定的函数
func (s *NatsService) SubDefaultHandle(nh interface{}) {
	switch nh.(type) {
	case ReqCtxHandler:
		s.defCtxHandler = nh.(ReqCtxHandler)
	case NatsHandler:
		s.defHandler = nh.(NatsHandler)
		s.defCtxHandler = compatibleNatsCtxHandler
	default:
		panic("handle func type error")
	}
}

func compatibleNatsCtxEventHandler(ctx *ReqCtx, reqmsg *RpcMsg) (rspmsg *RpcMsg, err error) {
	c, _ := ctx.Ctx.(*NatsService)

	if v, ok := c.eventMap.Load(ctx.Cmd); ok {
		if fn, _ := v.(NatsHandler); fn != nil {
			fn(c, ctx.Cmd, reqmsg)
		}
	}

	return
}

//事件都是共享的
func (s *NatsService) SubEventHandle(modcmd string, nh interface{}) {
	if nh == nil {
		s.eventMap.Delete(modcmd)
		s.ctxEventMap.Delete(modcmd)
		return
	}

	switch nh.(type) {
	case ReqCtxHandler:
		s.ctxEventMap.Store(modcmd, nh)
	case NatsHandler:
		s.eventMap.Store(modcmd, nh)
		s.ctxEventMap.Store(modcmd, compatibleNatsCtxEventHandler)
	default:
		panic("handle func type error")
	}

	subj := fmt.Sprintf("evt.%s", modcmd)
	// s.conn.QueueSubscribe(subj, s.svrmark, func(msg *nats.Msg) {
	// 	s.natsHandler(msg)
	// })
	s.conn.QueueSubscribeSyncWithChan(subj, s.svrmark, s.ch_recv)
	s.conn.Flush()
}

func (s *NatsService) PubSubjData(subj string, data []byte) error {
	if !s.checkServiceSubscriptions(subj) {
		// LogD("no service, %s, now svrs:%v", subj, s.services)
		return errors.New("no service")
	}
	nmsg := &natsMsg{subj: subj, data: data}
	return s.ch.PushMsg(nmsg)
}
func (s *NatsService) PubSubjMsg(subj string, msg *RpcMsg) error {
	if !s.checkServiceSubscriptions(subj) {
		// LogD("no service, %s, now svrs:%v", subj, s.services)
		return errors.New("no service")
	}
	//内部不要做JsonEncode
	// nmsg.rpcmsg.Para = []byte(JsonEncode(string(nmsg.rpcmsg.Para)))
	data, err := jsoniter.Marshal(msg)
	if err != nil || len(data) == 0 {
		return NewError("Marshal err:%v, datalen=%d", err, len(data))
	}

	nmsg := &natsMsg{subj: subj, data: data}
	return s.ch.PushMsg(nmsg)
}

//仅发出，不期望任何回复
func (s *NatsService) PubMsg(modcmd string, msg *RpcMsg) error {
	subj := fmt.Sprintf("req.%s", modcmd)

	return s.PubSubjMsg(subj, msg)
}

//发出，期望回复时调用rspChanHandler，chseq作为分发依据
func (s *NatsService) PubByChannel(modcmd string, chseq uint64, msg *RpcMsg) error {
	if chseq >= nats_low_watermark {
		return errors.New("chseq more then 1<<62")
	}
	subj := fmt.Sprintf("req.%s.%s.%d", modcmd, s.svrmark, chseq)

	// if !s.checkServiceSubscriptions(subj) {
	// 	LogD("no service, %s, now svrs:%v", subj, s.services)
	// 	return errors.New("no service")
	// }

	return s.PubSubjMsg(subj, msg)
}

//发出，并等待回复再返回
func (s *NatsService) PubWithResponse(modcmd string, msg *RpcMsg, sec int64) (rsp *RpcMsg, err error) {
	sndid := atomic.AddUint64(&s.sndid, 1)
	subj := fmt.Sprintf("req.%s.%s.%d", modcmd, s.svrmark, sndid)

	// if !s.checkServiceSubscriptions(subj) {
	// 	LogD("no service, %s, now svrs:%v", subj, s.services)
	// 	return nil, errors.New("no service")
	// }

	ch := make(chan *RpcMsg)
	defer close(ch)

	s.sndmap.Store(sndid, ch)
	defer s.sndmap.Delete(sndid)

	if err = s.PubSubjMsg(subj, msg); err != nil {
		return
	}

	select {
	case rsp = <-ch:
		return rsp, nil
	case <-time.After(time.Second * time.Duration(sec)):
		return nil, errors.New("timeout")
	}
}
func (s *NatsService) PubParseResponse(modcmd string, msg *RpcMsg, sec int64, pret interface{}) (err error) {
	rsp, err := s.PubWithResponse(modcmd, msg, sec)
	if err != nil {
		return err
	}
	if rsp.Err != nil {
		return errors.New(fmt.Sprintf("%s-%s", rsp.Err.Ret, rsp.Err.Msg))
	}
	if pret != nil {
		err = jsoniter.Unmarshal(rsp.Para, pret)
	}
	return
}

func (s *NatsService) RpcRequest(msg *RpcMsg, pret interface{}) (err error) {
	if msg.Sess == nil {
		return errors.New("rpc need sess")
	}
	//rpc 3秒足够了，若有超时应用，则自己封装吧
	return s.PubParseResponse(msg.Cmd, msg, 3, pret)
}

//广播模块的事件。nats主题是分发依据
func (s *NatsService) PubEvent(natscmd string, msg *RpcMsg) error {
	subj := fmt.Sprintf("evt.%s", natscmd)
	if n := strings.LastIndex(natscmd, "."); n == -1 {
		subj = fmt.Sprintf("evt.%s.%s", s.svrname, natscmd)
		// natscmd = string([]byte(natscmd)[n+1:])
	}
	// LogD("%s, want PUB %s", s.svrname, subj)

	return s.PubSubjMsg(subj, msg)
}
