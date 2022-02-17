// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xds

import (
	"errors"
	"fmt"
	"strings"
	"time"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"istio.io/istio/pilot/pkg/features"
	istiogrpc "istio.io/istio/pilot/pkg/grpc"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pilot/pkg/util/sets"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	istiolog "istio.io/pkg/log"
)

var deltaLog = istiolog.RegisterScope("delta", "delta xds debugging", 0)

func (s *DiscoveryServer) StreamDeltas(stream DeltaDiscoveryStream) error {
	if knativeEnv != "" && firstRequest.Load() {
		// How scaling works in knative is the first request is the "loading" request. During
		// loading request, concurrency=1. Once that request is done, concurrency is enabled.
		// However, the XDS stream is long lived, so the first request would block all others. As a
		// result, we should exit the first request immediately; clients will retry.
		firstRequest.Store(false)
		return status.Error(codes.Unavailable, "server warmup not complete; try again")
	}
	// Check if server is ready to accept clients and process new requests.
	// Currently ready means caches have been synced and hence can build
	// clusters correctly. Without this check, InitContext() call below would
	// initialize with empty config, leading to reconnected Envoys loosing
	// configuration. This is an additional safety check inaddition to adding
	// cachesSynced logic to readiness probe to handle cases where kube-proxy
	// ip tables update latencies.
	// See https://github.com/istio/istio/issues/25495.
	if !s.IsServerReady() {
		return errors.New("server is not ready to serve discovery information")
	}

	ctx := stream.Context()
	peerAddr := "0.0.0.0"
	if peerInfo, ok := peer.FromContext(ctx); ok {
		peerAddr = peerInfo.Addr.String()
	}

	if err := s.WaitForRequestLimit(stream.Context()); err != nil {
		deltaLog.Warnf("ADS: %q exceeded rate limit: %v", peerAddr, err)
		return status.Errorf(codes.ResourceExhausted, "request rate limit exceeded: %v", err)
	}

	ids, err := s.authenticate(ctx)
	if err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}
	if ids != nil {
		deltaLog.Debugf("Authenticated XDS: %v with identity %v", peerAddr, ids)
	} else {
		deltaLog.Debugf("Unauthenticated XDS: %v", peerAddr)
	}

	// InitContext returns immediately if the context was already initialized.
	if err = s.globalPushContext().InitContext(s.Env, nil, nil); err != nil {
		// Error accessing the data - log and close, maybe a different pilot replica
		// has more luck
		deltaLog.Warnf("Error reading config %v", err)
		return status.Error(codes.Unavailable, "error reading config")
	}
	con := newDeltaConnection(peerAddr, stream)

	// Do not call: defer close(con.pushChannel). The push channel will be garbage collected
	// when the connection is no longer used. Closing the channel can cause subtle race conditions
	// with push. According to the spec: "It's only necessary to close a channel when it is important
	// to tell the receiving goroutines that all data have been sent."

	// Block until either a request is received or a push is triggered.
	// We need 2 go routines because 'read' blocks in Recv().
	go s.receiveDelta(con, ids)

	// Wait for the proxy to be fully initialized before we start serving traffic. Because
	// initialization doesn't have dependencies that will block, there is no need to add any timeout
	// here. Prior to this explicit wait, we were implicitly waiting by receive() not sending to
	// reqChannel and the connection not being enqueued for pushes to pushChannel until the
	// initialization is complete.
	<-con.initialized

	for {
		select {
		case req, ok := <-con.deltaReqChan:
			if ok {
				deltaLog.Debugf("ADS: got Delta Request for: %s", req.TypeUrl)
				if err := s.processDeltaRequest(req, con); err != nil {
					return err
				}
			} else {
				// Remote side closed connection or error processing the request.
				return <-con.errorChan
			}
		case pushEv := <-con.pushChannel:
			err := s.pushConnectionDelta(con, pushEv)
			pushEv.done()
			if err != nil {
				return err
			}
		case <-con.stop:
			return nil
		}
	}
}

// Compute and send the new configuration for a connection. This is blocking and may be slow
// for large configs. The method will hold a lock on con.pushMutex.
func (s *DiscoveryServer) pushConnectionDelta(con *Connection, pushEv *Event) error {
	pushRequest := pushEv.pushRequest

	if pushRequest.Full {
		// Update Proxy with current information.
		s.updateProxy(con.proxy, pushRequest)
	}

	if !s.ProxyNeedsPush(con.proxy, pushRequest) {
		deltaLog.Debugf("Skipping push to %v, no updates required", con.ConID)
		if pushRequest.Full {
			// Only report for full versions, incremental pushes do not have a new version
			reportAllEvents(s.StatusReporter, con.ConID, pushRequest.Push.LedgerVersion, nil)
		}
		return nil
	}

	// Send pushes to all generators
	// Each Generator is responsible for determining if the push event requires a push
	wrl, ignoreEvents := con.pushDetails()
	for _, w := range wrl {
		if !features.EnableFlowControl {
			// Always send the push if flow control disabled
			if err := s.pushDeltaXds(con, pushRequest.Push, w, nil, pushRequest); err != nil {
				return err
			}
			continue
		}
		// If flow control is enabled, we will only push if we got an ACK for the previous response
		synced, timeout := con.Synced(w.TypeUrl)
		if !synced && timeout {
			// We are not synced, but we have been stuck for too long. We will trigger the push anyways to
			// avoid any scenario where this may deadlock.
			// This can possibly be removed in the future if we find this never causes issues
			totalDelayedPushes.With(typeTag.Value(v3.GetMetricType(w.TypeUrl))).Increment()
			deltaLog.Warnf("%s: QUEUE TIMEOUT for node:%s", v3.GetShortType(w.TypeUrl), con.proxy.ID)
		}
		if synced || timeout {
			// Send the push now
			if err := s.pushDeltaXds(con, pushRequest.Push, w, nil, pushRequest); err != nil {
				return err
			}
		} else {
			// The type is not yet synced. Instead of pushing now, which may overload Envoy,
			// we will wait until the last push is ACKed and trigger the push. See
			// https://github.com/istio/istio/issues/25685 for details on the performance
			// impact of sending pushes before Envoy ACKs.
			totalDelayedPushes.With(typeTag.Value(v3.GetMetricType(w.TypeUrl))).Increment()
			deltaLog.Debugf("%s: QUEUE for node:%s", v3.GetShortType(w.TypeUrl), con.proxy.ID)
			con.proxy.Lock()
			con.blockedPushes[w.TypeUrl] = con.blockedPushes[w.TypeUrl].CopyMerge(pushEv.pushRequest)
			con.proxy.Unlock()
		}
	}
	if pushRequest.Full {
		// Report all events for unwatched resources. Watched resources will be reported in pushXds or on ack.
		reportAllEvents(s.StatusReporter, con.ConID, pushRequest.Push.LedgerVersion, ignoreEvents)
	}

	proxiesConvergeDelay.Record(time.Since(pushRequest.Start).Seconds())
	return nil
}

func (s *DiscoveryServer) receiveDelta(con *Connection, identities []string) {
	defer func() {
		close(con.deltaReqChan)
		close(con.errorChan)
		// Close the initialized channel, if its not already closed, to prevent blocking the stream
		select {
		case <-con.initialized:
		default:
			close(con.initialized)
		}
	}()
	firstRequest := true
	for {
		req, err := con.deltaStream.Recv()
		if err != nil {
			if istiogrpc.IsExpectedGRPCError(err) {
				deltaLog.Infof("ADS: %q %s terminated %v", con.PeerAddr, con.ConID, err)
				return
			}
			con.errorChan <- err
			deltaLog.Errorf("ADS: %q %s terminated with error: %v", con.PeerAddr, con.ConID, err)
			totalXDSInternalErrors.Increment()
			return
		}
		// This should be only set for the first request. The node id may not be set - for example malicious clients.
		if firstRequest {
			firstRequest = false
			if req.Node == nil || req.Node.Id == "" {
				con.errorChan <- status.New(codes.InvalidArgument, "missing node information").Err()
				return
			}
			if err := s.initConnection(req.Node, con, identities); err != nil {
				con.errorChan <- err
				return
			}
			defer s.closeConnection(con)
			deltaLog.Infof("ADS: new delta connection for node:%s", con.ConID)
		}

		select {
		case con.deltaReqChan <- req:
		case <-con.deltaStream.Context().Done():
			deltaLog.Infof("ADS: %q %s terminated with stream closed", con.PeerAddr, con.ConID)
			return
		}
	}
}

func (conn *Connection) sendDelta(res *discovery.DeltaDiscoveryResponse) error {
	sendHandler := func() error {
		start := time.Now()
		defer func() { recordSendTime(time.Since(start)) }()
		return conn.deltaStream.Send(res)
	}
	err := istiogrpc.Send(conn.deltaStream.Context(), sendHandler)
	if err == nil {
		sz := 0
		for _, rc := range res.Resources {
			sz += len(rc.Resource.Value)
		}
		if res.Nonce != "" && !strings.HasPrefix(res.TypeUrl, v3.DebugType) {
			conn.proxy.Lock()
			if conn.proxy.WatchedResources[res.TypeUrl] == nil {
				conn.proxy.WatchedResources[res.TypeUrl] = &model.WatchedResource{TypeUrl: res.TypeUrl}
			}
			conn.proxy.WatchedResources[res.TypeUrl].NonceSent = res.Nonce
			conn.proxy.WatchedResources[res.TypeUrl].VersionSent = res.SystemVersionInfo
			conn.proxy.WatchedResources[res.TypeUrl].LastSent = time.Now()
			conn.proxy.Unlock()
		}
	} else {
		deltaLog.Infof("Timeout writing %s", conn.ConID)
		xdsResponseWriteTimeouts.Increment()
	}
	return err
}

// processDeltaRequest is handling one request. This is currently called from the 'main' thread, which also
// handles 'push' requests and close - the code will eventually call the 'push' code, and it needs more mutex
// protection. Original code avoided the mutexes by doing both 'push' and 'process requests' in same thread.
func (s *DiscoveryServer) processDeltaRequest(req *discovery.DeltaDiscoveryRequest, con *Connection) error {
	if !s.shouldProcessRequest(con.proxy, deltaToSotwRequest(req)) {
		return nil
	}
	if strings.HasPrefix(req.TypeUrl, v3.DebugType) {
		return s.pushXds(con, s.globalPushContext(), &model.WatchedResource{
			TypeUrl: req.TypeUrl, ResourceNames: req.ResourceNamesSubscribe,
		}, &model.PushRequest{Full: true})
	}
	if s.StatusReporter != nil {
		s.StatusReporter.RegisterEvent(con.ConID, req.TypeUrl, req.ResponseNonce)
	}
	shouldRespond := s.shouldRespondDelta(con, req)
	var request *model.PushRequest
	push := s.globalPushContext()
	if shouldRespond {
		// This is a request, trigger a full push for this type. Override the blocked push (if it exists),
		// as this full push is guaranteed to be a superset of what we would have pushed from the blocked push.
		request = &model.PushRequest{Full: true, Push: push}
	} else {
		// Check if we have a blocked push. If this was an ACK, we will send it.
		// Either way we remove the blocked push as we will send a push.
		haveBlockedPush := false
		con.proxy.Lock()
		request, haveBlockedPush = con.blockedPushes[req.TypeUrl]
		delete(con.blockedPushes, req.TypeUrl)
		con.proxy.Unlock()
		if haveBlockedPush {
			// we have a blocked push which we will use
			deltaLog.Debugf("%s: DEQUEUE for node:%s", v3.GetShortType(req.TypeUrl), con.proxy.ID)
		} else {
			// This is an ACK, no delayed push
			// Return immediately, no action needed
			return nil
		}
	}

	request.Reason = append(request.Reason, model.ProxyRequest)
	request.Start = time.Now()
	// SidecarScope for the proxy may has not been updated based on this pushContext.
	// It can happen when `processRequest` comes after push context has been updated(s.initPushContext),
	// but before proxy's SidecarScope has been updated(s.updateProxy).
	if con.proxy.SidecarScope != nil && con.proxy.SidecarScope.Version != push.PushVersion {
		s.computeProxyState(con.proxy, request)
	}
	return s.pushDeltaXds(con, push, con.Watched(req.TypeUrl), req.ResourceNamesSubscribe, request)
}

// shouldRespondDelta determines whether this request needs to be responded back. It applies the ack/nack rules as per xds protocol
// using WatchedResource for previous state and discovery request for the current state.
func (s *DiscoveryServer) shouldRespondDelta(con *Connection, request *discovery.DeltaDiscoveryRequest) bool {
	stype := v3.GetShortType(request.TypeUrl)

	// If there is an error in request that means previous response is erroneous.
	// We do not have to respond in that case. In this case request's version info
	// will be different from the version sent. But it is fragile to rely on that.
	if request.ErrorDetail != nil {
		errCode := codes.Code(request.ErrorDetail.Code)
		deltaLog.Warnf("ADS:%s: ACK ERROR %s %s:%s", stype, con.ConID, errCode.String(), request.ErrorDetail.GetMessage())
		incrementXDSRejects(request.TypeUrl, con.proxy.ID, errCode.String())
		if s.StatusGen != nil {
			s.StatusGen.OnNack(con.proxy, deltaToSotwRequest(request))
		}
		con.proxy.Lock()
		if w, f := con.proxy.WatchedResources[request.TypeUrl]; f {
			w.NonceNacked = request.ResponseNonce
		}
		con.proxy.Unlock()
		return false
	}

	con.proxy.RLock()
	previousInfo := con.proxy.WatchedResources[request.TypeUrl]
	con.proxy.RUnlock()

	// This can happen in two cases:
	// 1. Envoy initially send request to Istiod
	// 2. Envoy reconnect to Istiod i.e. Istiod does not have
	// information about this typeUrl, but Envoy sends response nonce - either
	// because Istiod is restarted or Envoy disconnects and reconnects.
	// We should always respond with the current resource names.
	if previousInfo == nil {
		// TODO: can we distinguish init and reconnect? Do we care?
		deltaLog.Debugf("ADS:%s: INIT/RECONNECT %s %s", stype, con.ConID, request.ResponseNonce)
		con.proxy.Lock()
		con.proxy.WatchedResources[request.TypeUrl] = &model.WatchedResource{
			TypeUrl:       request.TypeUrl,
			ResourceNames: deltaWatchedResources(nil, request),
		}
		con.proxy.Unlock()
		return true
	}

	// If there is mismatch in the nonce, that is a case of expired/stale nonce.
	// A nonce becomes stale following a newer nonce being sent to Envoy.
	// TODO: due to concurrent unsubscribe, this probably doesn't make sense. Do we need any logic here?
	if request.ResponseNonce != "" && request.ResponseNonce != previousInfo.NonceSent {
		deltaLog.Debugf("ADS:%s: REQ %s Expired nonce received %s, sent %s", stype,
			con.ConID, request.ResponseNonce, previousInfo.NonceSent)
		xdsExpiredNonce.With(typeTag.Value(v3.GetMetricType(request.TypeUrl))).Increment()
		con.proxy.Lock()
		con.proxy.WatchedResources[request.TypeUrl].NonceNacked = ""
		con.proxy.Unlock()
		return false
	}

	// If it comes here, that means nonce match. This an ACK. We should record
	// the ack details and respond if there is a change in resource names.
	con.proxy.Lock()
	previousResources := con.proxy.WatchedResources[request.TypeUrl].ResourceNames
	deltaResources := deltaWatchedResources(previousResources, request)
	con.proxy.WatchedResources[request.TypeUrl].NonceAcked = request.ResponseNonce
	con.proxy.WatchedResources[request.TypeUrl].NonceNacked = ""
	con.proxy.WatchedResources[request.TypeUrl].ResourceNames = deltaResources
	con.proxy.Unlock()

	oldAck := listEqualUnordered(previousResources, deltaResources)
	// Spontaneous DeltaDiscoveryRequests from the client.
	// This can be done to dynamically add or remove elements from the tracked resource_names set.
	// In this case response_nonce is empty.
	newAck := request.ResponseNonce != ""
	if newAck != oldAck {
		// Not sure which is better, lets just log if they don't match for now and compare.
		deltaLog.Errorf("ADS:%s: New ACK and old ACK check mismatch: %v vs %v", stype, newAck, oldAck)
		if features.EnableUnsafeAssertions {
			panic(fmt.Sprintf("ADS:%s: New ACK and old ACK check mismatch: %v vs %v", stype, newAck, oldAck))
		}
	}
	// Envoy can send two DiscoveryRequests with same version and nonce
	// when it detects a new resource. We should respond if they change.
	if oldAck {
		deltaLog.Debugf("ADS:%s: ACK  %s %s", stype, con.ConID, request.ResponseNonce)
		return false
	}
	deltaLog.Debugf("ADS:%s: RESOURCE CHANGE previous resources: %v, new resources: %v %s %s", stype,
		previousResources, deltaResources, con.ConID, request.ResponseNonce)

	return true
}

// Push an XDS resource for the given connection. Configuration will be generated
// based on the passed in generator. Based on the updates field, generators may
// choose to send partial or even no response if there are no changes.
func (s *DiscoveryServer) pushDeltaXds(con *Connection, push *model.PushContext,
	w *model.WatchedResource, subscribe []string, req *model.PushRequest) error {
	if w == nil {
		return nil
	}
	gen := s.findGenerator(w.TypeUrl, con)
	if gen == nil {
		return nil
	}
	t0 := time.Now()

	// If subscribe is set, client is requesting specific resources. We should just generate the
	// new resources it needs, rather than the entire set of known resources.
	if subscribe != nil {
		w = &model.WatchedResource{
			TypeUrl:       w.TypeUrl,
			ResourceNames: subscribe,
		}
	}

	var res model.Resources
	var deletedRes model.DeletedResources
	var logdata model.XdsLogDetails
	var usedDelta bool
	var err error
	switch g := gen.(type) {
	case model.XdsDeltaResourceGenerator:
		res, deletedRes, logdata, usedDelta, err = g.GenerateDeltas(con.proxy, push, req, w)
	case model.XdsResourceGenerator:
		res, logdata, err = g.Generate(con.proxy, push, w, req)
	}
	if err != nil || (res == nil && deletedRes == nil) {
		// If we have nothing to send, report that we got an ACK for this version.
		if s.StatusReporter != nil {
			s.StatusReporter.RegisterEvent(con.ConID, w.TypeUrl, push.LedgerVersion)
		}
		return err
	}
	defer func() { recordPushTime(w.TypeUrl, time.Since(t0)) }()
	resp := &discovery.DeltaDiscoveryResponse{
		ControlPlane: ControlPlane(),
		TypeUrl:      w.TypeUrl,
		// TODO: send different version for incremental eds
		SystemVersionInfo: push.PushVersion,
		Nonce:             nonce(push.LedgerVersion),
		Resources:         res,
	}
	currentResources := extractNames(res)
	if usedDelta {
		resp.RemovedResources = deletedRes
	} else if req.Full {
		// similar to sotw
		subscribed := sets.NewSet(w.ResourceNames...)
		subscribed.Delete(currentResources...)
		resp.RemovedResources = subscribed.SortedList()
	}
	if len(resp.RemovedResources) > 0 {
		deltaLog.Debugf("ADS:%v %s REMOVE %v", v3.GetShortType(w.TypeUrl), con.ConID, resp.RemovedResources)
	}
	// normally wildcard xds `subscribe` is always nil, just in case there are some extended type not handled correctly.
	if subscribe == nil && isWildcardTypeURL(w.TypeUrl) {
		// this is probably a bad idea...
		con.proxy.Lock()
		w.ResourceNames = currentResources
		con.proxy.Unlock()
	}

	configSize := ResourceSize(res)
	configSizeBytes.With(typeTag.Value(w.TypeUrl)).Record(float64(configSize))

	ptype := "PUSH"
	info := ""
	if logdata.Incremental {
		ptype = "PUSH INC"
	}
	if len(logdata.AdditionalInfo) > 0 {
		info = " " + logdata.AdditionalInfo
	}

	if err := con.sendDelta(resp); err != nil {
		if recordSendError(w.TypeUrl, err) {
			deltaLog.Warnf("%s: Send failure for node:%s resources:%d size:%s%s: %v",
				v3.GetShortType(w.TypeUrl), con.proxy.ID, len(res), util.ByteCount(configSize), info, err)
		}
		return err
	}

	switch {
	case logdata.Incremental:
		if deltaLog.DebugEnabled() {
			deltaLog.Debugf("%s: %s%s for node:%s resources:%d size:%s%s",
				v3.GetShortType(w.TypeUrl), ptype, req.PushReason(), con.proxy.ID, len(res), util.ByteCount(configSize), info)
		}
	default:
		debug := ""
		if deltaLog.DebugEnabled() {
			// Add additional information to logs when debug mode enabled.
			debug = " nonce:" + resp.Nonce + " version:" + resp.SystemVersionInfo
		}
		deltaLog.Infof("%s: %s%s for node:%s resources:%d size:%v%s%s", v3.GetShortType(w.TypeUrl), ptype, req.PushReason(), con.proxy.ID, len(res),
			util.ByteCount(ResourceSize(res)), info, debug)
	}

	return nil
}

func newDeltaConnection(peerAddr string, stream DeltaDiscoveryStream) *Connection {
	return &Connection{
		pushChannel:   make(chan *Event),
		initialized:   make(chan struct{}),
		stop:          make(chan struct{}),
		PeerAddr:      peerAddr,
		Connect:       time.Now(),
		deltaStream:   stream,
		deltaReqChan:  make(chan *discovery.DeltaDiscoveryRequest, 1),
		errorChan:     make(chan error, 1),
		blockedPushes: map[string]*model.PushRequest{},
	}
}

// To satisfy methods that need DiscoveryRequest. Not suitable for real usage
func deltaToSotwRequest(request *discovery.DeltaDiscoveryRequest) *discovery.DiscoveryRequest {
	return &discovery.DiscoveryRequest{
		Node:          request.Node,
		ResourceNames: request.ResourceNamesSubscribe,
		TypeUrl:       request.TypeUrl,
		ResponseNonce: request.ResponseNonce,
		ErrorDetail:   request.ErrorDetail,
	}
}

func deltaWatchedResources(existing []string, request *discovery.DeltaDiscoveryRequest) []string {
	res := sets.NewSet(existing...)
	res.Insert(request.ResourceNamesSubscribe...)
	res.Delete(request.ResourceNamesUnsubscribe...)
	return res.SortedList()
}

func extractNames(res []*discovery.Resource) []string {
	names := []string{}
	for _, r := range res {
		names = append(names, r.Name)
	}
	return names
}
