/*
 * Copyright 2021 CloudWeGo Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package nphttp2

import (
	"context"
	"fmt"
	"net"
	"runtime/debug"
	"strings"

	"github.com/cloudwego/netpoll"

	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/gofunc"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/remote"
	"github.com/cloudwego/kitex/pkg/remote/codec/protobuf"
	"github.com/cloudwego/kitex/pkg/remote/trans/nphttp2/codes"
	grpcTransport "github.com/cloudwego/kitex/pkg/remote/trans/nphttp2/grpc"
	"github.com/cloudwego/kitex/pkg/remote/trans/nphttp2/status"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/pkg/serviceinfo"
	"github.com/cloudwego/kitex/pkg/streaming"
	"github.com/cloudwego/kitex/transport"
)

type svrTransHandlerFactory struct{}

// NewSvrTransHandlerFactory ...
func NewSvrTransHandlerFactory() remote.ServerTransHandlerFactory {
	return &svrTransHandlerFactory{}
}

func (f *svrTransHandlerFactory) NewTransHandler(opt *remote.ServerOption) (remote.ServerTransHandler, error) {
	return newSvrTransHandler(opt)
}

func newSvrTransHandler(opt *remote.ServerOption) (*svrTransHandler, error) {
	return &svrTransHandler{
		opt:     opt,
		svcInfo: opt.SvcInfo,
		codec:   protobuf.NewGRPCCodec(),
	}, nil
}

var _ remote.ServerTransHandler = &svrTransHandler{}

type svrTransHandler struct {
	opt        *remote.ServerOption
	svcInfo    *serviceinfo.ServiceInfo
	inkHdlFunc endpoint.Endpoint
	codec      remote.Codec
}

func (t *svrTransHandler) Write(ctx context.Context, conn net.Conn, msg remote.Message) (err error) {
	buf := newBuffer(conn)
	defer buf.Release(err)

	if err = t.codec.Encode(ctx, msg, buf); err != nil {
		return err
	}
	return buf.Flush()
}

func (t *svrTransHandler) Read(ctx context.Context, conn net.Conn, msg remote.Message) (err error) {
	buf := newBuffer(conn)
	defer buf.Release(err)
	err = t.codec.Decode(ctx, msg, buf)
	return
}

// 只 return write err
func (t *svrTransHandler) OnRead(ctx context.Context, conn net.Conn) error {
	tr, err := grpcTransport.NewServerTransport(ctx, conn.(netpoll.Connection))
	if err != nil {
		return err
	}
	defer tr.Close()

	tr.HandleStreams(func(s *grpcTransport.Stream) {
		gofunc.GoFunc(ctx, func() {
			ri, ctx := t.opt.InitRPCInfoFunc(s.Context(), tr.RemoteAddr())
			// set grpc transport flag before excute metahandler
			rpcinfo.AsMutableRPCConfig(ri.Config()).SetTransportProtocol(transport.GRPC)
			var err error
			for _, shdlr := range t.opt.StreamingMetaHandlers {
				ctx, err = shdlr.OnReadStream(ctx)
				if err != nil {
					tr.WriteStatus(s, convertFromKitexToGrpc(err))
					return
				}
			}
			ctx = t.startTracer(ctx, ri)
			defer func() {
				panicErr := recover()
				if panicErr != nil {
					if conn != nil {
						t.opt.Logger.Errorf("KITEX: panic happened, close conn[%s], %v\n%s", conn.RemoteAddr(), panicErr, string(debug.Stack()))
					} else {
						t.opt.Logger.Errorf("KITEX: panic happened, %v\n%s", panicErr, string(debug.Stack()))
					}
				}
				t.finishTracer(ctx, ri, err, panicErr)
			}()

			ink := ri.Invocation().(rpcinfo.InvocationSetter)
			sm := s.Method()
			if sm != "" && sm[0] == '/' {
				sm = sm[1:]
			}
			pos := strings.LastIndex(sm, "/")
			if pos == -1 {
				errDesc := fmt.Sprintf("malformed method name: %q", s.Method())
				tr.WriteStatus(s, status.New(codes.ResourceExhausted, errDesc))
				return
			}
			ink.SetMethodName(sm[pos+1:])

			idx := strings.LastIndex(sm[:pos], ".")
			if idx == -1 {
				errDesc := fmt.Sprintf("malformed package and service name: %q", s.Method())
				tr.WriteStatus(s, status.New(codes.ResourceExhausted, errDesc))
				return
			}
			ink.SetPackageName(sm[:idx])
			ink.SetServiceName(sm[idx+1 : pos])

			st := NewStream(ctx, t.svcInfo, newServerConn(tr, s), t)
			if err := t.inkHdlFunc(ctx, &streaming.Args{Stream: st}, nil); err != nil {
				tr.WriteStatus(s, convertFromKitexToGrpc(err))
				return
			}
			tr.WriteStatus(s, status.New(codes.OK, ""))
		})
	}, func(ctx context.Context, method string) context.Context {
		return ctx
	})
	return nil
}

// msg 是解码后的实例，如 Arg 或 Result, 触发上层处理，用于异步 和 服务端处理
func (t *svrTransHandler) OnMessage(ctx context.Context, args, result remote.Message) (context.Context, error) {
	panic("unimplemented")
}

// 新连接建立时触发，主要用于服务端，对应 netpoll onPrepare
func (t *svrTransHandler) OnActive(ctx context.Context, conn net.Conn) (context.Context, error) {
	// set readTimeout to infinity to avoid streaming break
	// use keepalive to check the health of connection
	conn.(netpoll.Connection).SetReadTimeout(grpcTransport.Infinity)
	return ctx, nil
}

// 连接关闭时回调
func (t *svrTransHandler) OnInactive(ctx context.Context, conn net.Conn) {
	// recycle rpcinfo
	rpcinfo.PutRPCInfo(rpcinfo.GetRPCInfo(ctx))
}

// 传输层 error 回调
func (t *svrTransHandler) OnError(ctx context.Context, err error, conn net.Conn) {
	if pe, ok := err.(*kerrors.DetailedError); ok {
		t.opt.Logger.Errorf("KITEX: processing request error, remote=%s, err=%s\n%s", conn.RemoteAddr(), err.Error(), pe.Stack())
	} else {
		t.opt.Logger.Errorf("KITEX: processing request error, remote=%s, err=%s", conn.RemoteAddr(), err.Error())
	}
}

func (t *svrTransHandler) SetInvokeHandleFunc(inkHdlFunc endpoint.Endpoint) {
	t.inkHdlFunc = inkHdlFunc
}

func (t *svrTransHandler) SetPipeline(p *remote.TransPipeline) {
}

func (t *svrTransHandler) startTracer(ctx context.Context, ri rpcinfo.RPCInfo) context.Context {
	c := t.opt.TracerCtl.DoStart(ctx, ri, t.opt.Logger)
	return c
}

func (t *svrTransHandler) finishTracer(ctx context.Context, ri rpcinfo.RPCInfo, err error, panicErr interface{}) {
	rpcStats := rpcinfo.AsMutableRPCStats(ri.Stats())
	if rpcStats == nil {
		return
	}
	if panicErr != nil {
		rpcStats.SetPanicked(panicErr)
	}
	t.opt.TracerCtl.DoFinish(ctx, ri, err, t.opt.Logger)
	rpcStats.Reset()
}
