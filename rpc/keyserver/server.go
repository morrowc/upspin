// Copyright 2016 The Upspin Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package keyserver is a wrapper for an upspin.KeyServer implementation
// that presents it as an authenticated service.
package keyserver // import "upspin.io/rpc/keyserver"

import (
	"expvar"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	pb "github.com/golang/protobuf/proto"

	"upspin.io/config"
	"upspin.io/errors"
	"upspin.io/log"
	"upspin.io/rpc"
	"upspin.io/serverutil"
	"upspin.io/upspin"
	"upspin.io/upspin/proto"
)

type server struct {
	config upspin.Config

	// What this server reports itself as through its Endpoint method.
	endpoint upspin.Endpoint

	// The underlying keyserver implementation.
	key upspin.KeyServer

	// Counters for tracking Lookup load.
	lookupCounter [3]*serverutil.RateCounter

	// Counters for tracking Put load.
	putCounter [3]*serverutil.RateCounter
}

// How often to sample, where each sample is a second.
var defaultSampling = []int{10, 60, 300}

// New creates a new instance of the RPC key server.
func New(cfg upspin.Config, key upspin.KeyServer, addr upspin.NetAddr) http.Handler {
	s := &server{
		config: cfg,
		endpoint: upspin.Endpoint{
			Transport: upspin.Remote,
			NetAddr:   addr,
		},
		key: key,
	}
	s.registerCounters()
	return rpc.NewServer(cfg, rpc.Service{
		Name: "Key",
		Methods: map[string]rpc.Method{
			"Put": s.Put,
		},
		UnauthenticatedMethods: map[string]rpc.UnauthenticatedMethod{
			"Lookup": s.Lookup,
		},
		Lookup: func(userName upspin.UserName) (upspin.PublicKey, error) {
			user, err := key.Lookup(userName)
			if err != nil {
				return "", err
			}
			return user.PublicKey, nil
		},
	})
}

func (s *server) registerCounters() {
	var err error
	for i, samples := range defaultSampling {
		s.lookupCounter[i], err = serverutil.NewRateCounter(samples, time.Second)
		if err != nil {
			panic(err)
		}
		expvar.Publish(fmt.Sprintf("lookup-%ds", samples), s.lookupCounter[i])
		s.putCounter[i], err = serverutil.NewRateCounter(samples, time.Second)
		if err != nil {
			panic(err)
		}
		expvar.Publish(fmt.Sprintf("put-%ds", samples), s.putCounter[i])
	}
}

func (s *server) incLookupCounters() {
	for i := range s.lookupCounter {
		s.lookupCounter[i].Add(1)
	}
}

func (s *server) incPutCounters() {
	for i := range s.putCounter {
		s.putCounter[i].Add(1)
	}
}

func (s *server) serverFor(session rpc.Session, reqBytes []byte, req pb.Message) (upspin.KeyServer, error) {
	if err := pb.Unmarshal(reqBytes, req); err != nil {
		return nil, err
	}
	svc, err := s.key.Dial(config.SetUserName(s.config, session.User()), s.key.Endpoint())
	if err != nil {
		return nil, err
	}
	return svc.(upspin.KeyServer), nil
}

// Lookup implements proto.KeyServer, and does not do any authentication.
func (s *server) Lookup(reqBytes []byte) (pb.Message, error) {
	var req proto.KeyLookupRequest
	if err := pb.Unmarshal(reqBytes, &req); err != nil {
		return nil, err
	}
	logfOnceInN(100, "Lookup(%q)", req.UserName)
	s.incLookupCounters()

	user, err := s.key.Lookup(upspin.UserName(req.UserName))
	if err != nil {
		logf(nil, "Lookup(%q) failed: %s", req.UserName, err)
		return &proto.KeyLookupResponse{Error: errors.MarshalError(err)}, nil
	}
	return &proto.KeyLookupResponse{User: proto.UserProto(user)}, nil
}

// Put implements proto.KeyServer.
func (s *server) Put(session rpc.Session, reqBytes []byte) (pb.Message, error) {
	var req proto.KeyPutRequest
	key, err := s.serverFor(session, reqBytes, &req)
	if err != nil {
		return nil, err
	}
	op := logf(session, "Put(%v)", req)
	s.incPutCounters()

	user := proto.UpspinUser(req.User)
	err = key.Put(user)
	if err != nil {
		op.log(err)
		return putError(err), nil
	}
	return &proto.KeyPutResponse{}, nil
}

func putError(err error) *proto.KeyPutResponse {
	return &proto.KeyPutResponse{Error: errors.MarshalError(err)}
}

// logOnceInN logs an operation probabilistically once for every n calls.
func logfOnceInN(n int, format string, args ...interface{}) {
	if n <= 1 || rand.Intn(n) == 0 {
		logf(nil, format, args...)
	}
}

func logf(sess rpc.Session, format string, args ...interface{}) operation {
	op := "rpc/keyserver: "
	if sess != nil {
		op += fmt.Sprintf("%q: ", sess.User())
	}
	op += "key." + fmt.Sprintf(format, args...)
	log.Debug.Print(op)
	return operation(op)
}

type operation string

func (op operation) log(err error) {
	log.Printf("%v failed: %v", op, err)
}
