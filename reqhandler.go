package oo

import (
	"fmt"
	"sort"
	"strings"
	// "sync"
)

//返回的err仅用于日志，若要回发消息，必须明确返回消息部分
type ReqCtxHandler = func(*ReqCtx, *RpcMsg) (*RpcMsg, error)

//请求的输入、输出过滤器
type ReqCtxFilter = func(*ReqCtx, *RpcMsg, int64) error

type ctxFilter = struct {
	Prio int
	Fn   ReqCtxFilter
}

type ctxHandler = struct {
	hFlag int64
	Fn    ReqCtxHandler
}

//请求分发器
type ReqDispatcher struct {
	hName      string
	inFilters  []*ctxFilter
	outFilters []*ctxFilter
	// handlersMap *sync.Map //cmd-->[]*ctxHandler
	handlersMap map[string]([]*ctxHandler)
	defHandler  ReqCtxHandler //在调用def时不会调用任何filter
}

func CreateReqDispatcher(name ...string) (cmap *ReqDispatcher) {
	cmap = &ReqDispatcher{}
	if len(name) > 0 {
		cmap.hName = name[0]
	}
	// cmap.handlersMap = new(sync.Map)
	cmap.handlersMap = map[string]([]*ctxHandler){}
	return
}
func MergeReqDispatcher(unique bool, cmaps ...*ReqDispatcher) (cmap *ReqDispatcher) {
	cmap = CreateReqDispatcher()
	cmap.Merge(unique, cmaps...)
	return
}
func (cmap *ReqDispatcher) Merge(unique bool, cmaps ...*ReqDispatcher) (cmap_ret *ReqDispatcher) {
	for _, cmap1 := range cmaps {
		cmap.hName += fmt.Sprintf(".%s", cmap1.hName)
		for cmd := range cmap1.handlersMap {
			if !unique || cmap.handlersMap[cmd] == nil {
				//不排它或未有，都可以加
				//属于嵌套调用
				cmap.AddHandler(cmd, cmap1.RunHandler)
			}
		}
		//依然按优先唯一
		if cmap.defHandler == nil {
			cmap.defHandler = cmap1.defHandler
		}
	}
	cmap_ret = cmap
	return
	// return MergeReqDispatcher(unique, append([]*ReqDispatcher{cmap}, cmaps...)...)
}

//hFlag是命令特征字 用于filter调用时，识别出此命令的特征，便于：校验session/检查登录/鉴权
//当fn是其它ReqDispatcher.RunHandler时，会作为闭包函数被调用
//支持组合调用，注意此时filter会每次都执行。
func (cmap *ReqDispatcher) AddHandler(cmd string, fn ReqCtxHandler, hFlags ...int64) {
	h := ctxHandler{
		Fn: fn,
	}
	if len(hFlags) > 0 {
		h.hFlag = hFlags[0]
	}
	if hs, ok := cmap.handlersMap[cmd]; ok {
		hs = append(hs, &h)
		cmap.handlersMap[cmd] = hs
	} else {
		cmap.handlersMap[cmd] = []*ctxHandler{&h}
	}
}

func (cmap *ReqDispatcher) SetDefHandler(fn ReqCtxHandler) {
	cmap.defHandler = fn
}
func (cmap *ReqDispatcher) AddInFilter(prio int, fn ReqCtxFilter) {
	cmap.inFilters = append(cmap.inFilters, &ctxFilter{
		Prio: prio,
		Fn:   fn,
	})
	sort.Slice(cmap.inFilters, func(i, j int) bool {
		return cmap.inFilters[i].Prio < cmap.inFilters[j].Prio
	})
}
func (cmap *ReqDispatcher) AddOutFilter(prio int, fn ReqCtxFilter) {
	cmap.outFilters = append(cmap.outFilters, &ctxFilter{
		Prio: prio,
		Fn:   fn,
	})
	sort.Slice(cmap.outFilters, func(i, j int) bool {
		return cmap.outFilters[i].Prio < cmap.outFilters[j].Prio
	})
}
func (cmap *ReqDispatcher) AppendDispatcher(cmaps ...*ReqDispatcher) {
	for _, cmap1 := range cmaps {
		cmap.hName += fmt.Sprintf(".%s", cmap1.hName)
		for cmd := range cmap1.handlersMap {
			if _, ok := cmap.handlersMap[cmd]; !ok {
				//属于嵌套调用
				cmap.AddHandler(cmd, cmap1.RunHandler)
			}
		}
		if cmap.defHandler == nil {
			cmap.defHandler = cmap1.defHandler
		}
	}
}

func (cmap *ReqDispatcher) runOneHanler(ph *ctxHandler, ctx *ReqCtx, reqmsg *RpcMsg) (rspmsg *RpcMsg, err error) {
	for _, ff := range cmap.inFilters {
		//重用输入消息
		if err = ff.Fn(ctx, reqmsg, ph.hFlag); err != nil {
			return
		}
	}

	if rspmsg, err = ph.Fn(ctx, reqmsg); err == nil && rspmsg != nil {
		//当有输出时，按优先顺序执行输出过滤函数链，当某个返回err，则停止并返回
		for _, ff := range cmap.outFilters {
			//注意，此时重用的是输出消息
			if err = ff.Fn(ctx, rspmsg, ph.hFlag); err != nil {
				return
			}
		}
	}
	return
}

//自己指定使用什么来分发
func (cmap *ReqDispatcher) RunHandlerEx(cmd string, ctx *ReqCtx, reqmsg *RpcMsg) (rspmsg *RpcMsg, err error) {
	//defer 出错由上层捕捉
	if phs, ok := cmap.handlersMap[cmd]; ok {
		if len(phs) == 1 {
			rspmsg, err = cmap.runOneHanler(phs[0], ctx, reqmsg)
			return
		}

		paras := []interface{}{}
		for i, ph := range phs {
			reqmsg1 := *reqmsg //复制避免内部重用reqmsg来返回给rspmsg
			rspmsg, err = cmap.runOneHanler(ph, ctx, &reqmsg1)
			if err == nil && rspmsg != nil && len(rspmsg.Para) > 0 {
				paras = append(paras, rspmsg.Para)
			}
			if err != nil {
				//也就只能日志一把。
				LogD("%s run %s[%d] flag %x err: %v", cmap.hName, cmd, i, ph.hFlag, err)
			}
		}
		//组合类不返回出错
		err = nil
		rspmsg = PackRspMsg(reqmsg, ESUCC, paras)
	} else if cmap.defHandler != nil {
		rspmsg, err = cmap.runOneHanler(&ctxHandler{Fn: cmap.defHandler}, ctx, reqmsg)
	} else {
		LogD("%s skip cmd %s", cmap.hName, cmd)
	}

	return
}

//强制用Cmd作为分发依据
func (cmap *ReqDispatcher) RunHandler(ctx *ReqCtx, reqmsg *RpcMsg) (rspmsg *RpcMsg, err error) {
	return cmap.RunHandlerEx(reqmsg.Cmd, ctx, reqmsg)
}

//用Mark作为分发依据
func (cmap *ReqDispatcher) RunHandlerMark(ctx *ReqCtx, reqmsg *RpcMsg) (rspmsg *RpcMsg, err error) {
	return cmap.RunHandlerEx(reqmsg.Mark, ctx, reqmsg)
}

func (cmap *ReqDispatcher) GetHandlersMap() map[string]([]*ctxHandler) {
	return cmap.handlersMap
}

func (cmap *ReqDispatcher) Print() (s string) {
	for _, f := range cmap.inFilters {
		s += fmt.Sprintf("infilter %d, %p\n", f.Prio, f.Fn)
	}

	for cmd, phs := range cmap.handlersMap {
		hans := []string{}
		for i, ph := range phs {
			hans = append(hans, fmt.Sprintf("(%d,0x%08x,%p)", i, ph.hFlag, ph.Fn))
		}
		s += fmt.Sprintf("handler %s-->[%s]\n", cmd, strings.Join(hans, ","))
	}

	for _, f := range cmap.outFilters {
		s += fmt.Sprintf("outfilter %d, %p\n", f.Prio, f.Fn)
	}
	return
}
