package gocsi

import (
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/thecodeteam/gocsi/csi"
	"google.golang.org/grpc"

	"golang.org/x/net/context"
)

// IdempotencyProvider is the interface that works with a server-side,
// gRPC interceptor to provide serial access and idempotency for CSI's
// volume resources.
type IdempotencyProvider interface {
	// GetVolumeID should return the ID of the volume specified
	// by the provided volume name. If the volume does not exist then
	// an empty string should be returned.
	GetVolumeID(ctx context.Context, name string) (string, error)

	// GetVolumeInfo should return information about the volume
	// specified by the provided volume ID or name. If the volume does not
	// exist then a nil value should be returned.
	GetVolumeInfo(ctx context.Context, id, name string) (*csi.VolumeInfo, error)

	// IsControllerPublished should return publication for a volume's
	// publication status on a specified node.
	IsControllerPublished(
		ctx context.Context,
		volumeID, nodeID string) (map[string]string, error)

	// IsNodePublished should return a flag indicating whether or
	// not the volume exists and is published on the current host.
	IsNodePublished(
		ctx context.Context,
		id string,
		pubVolInfo map[string]string,
		targetPath string) (bool, error)
}

// IdempotentInterceptorOption configures the idempotent interceptor.
type IdempotentInterceptorOption func(*idempIntercOpts)

type idempIntercOpts struct {
	timeout       time.Duration
	requireVolume bool
}

// WithIdempTimeout is an IdempotentInterceptorOption that sets the
// timeout used by the idempotent interceptor.
func WithIdempTimeout(t time.Duration) IdempotentInterceptorOption {
	return func(o *idempIntercOpts) {
		o.timeout = t
	}
}

// WithIdempRequireVolumeExists is an IdempotentInterceptorOption that
// enforces the requirement that volumes must exist before proceeding
// with an operation.
func WithIdempRequireVolumeExists() IdempotentInterceptorOption {
	return func(o *idempIntercOpts) {
		o.requireVolume = true
	}
}

// NewIdempotentInterceptor returns a new server-side, gRPC interceptor
// that can be used in conjunction with an IdempotencyProvider to
// provide serialized, idempotent access to the following CSI RPCs:
//
//  * CreateVolume
//  * DeleteVolume
//  * ControllerPublishVolume
//  * ControllerUnpublishVolume
//  * NodePublishVolume
//  * NodeUnpublishVolume
func NewIdempotentInterceptor(
	p IdempotencyProvider,
	opts ...IdempotentInterceptorOption) grpc.UnaryServerInterceptor {

	i := &idempotencyInterceptor{
		p:            p,
		volIDLocks:   map[string]*volLockInfo{},
		volNameLocks: map[string]*volLockInfo{},
	}

	// Configure the idempotent interceptor's options.
	for _, setOpt := range opts {
		setOpt(&i.opts)
	}

	return i.handle
}

type volLockInfo struct {
	MutexWithTryLock
	methodInErr map[string]struct{}
}

type idempotencyInterceptor struct {
	p             IdempotencyProvider
	volIDLocksL   sync.Mutex
	volNameLocksL sync.Mutex
	volIDLocks    map[string]*volLockInfo
	volNameLocks  map[string]*volLockInfo
	opts          idempIntercOpts
}

func (i *idempotencyInterceptor) lockWithID(id string) *volLockInfo {
	i.volIDLocksL.Lock()
	defer i.volIDLocksL.Unlock()
	lock := i.volIDLocks[id]
	if lock == nil {
		lock = &volLockInfo{
			MutexWithTryLock: NewMutexWithTryLock(),
			methodInErr:      map[string]struct{}{},
		}
		i.volIDLocks[id] = lock
	}
	return lock
}

func (i *idempotencyInterceptor) lockWithName(name string) *volLockInfo {
	i.volNameLocksL.Lock()
	defer i.volNameLocksL.Unlock()
	lock := i.volNameLocks[name]
	if lock == nil {
		lock = &volLockInfo{
			MutexWithTryLock: NewMutexWithTryLock(),
			methodInErr:      map[string]struct{}{},
		}
		i.volNameLocks[name] = lock
	}
	return lock
}

func (i *idempotencyInterceptor) handle(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (interface{}, error) {

	switch treq := req.(type) {
	case *csi.ControllerPublishVolumeRequest:
		return i.controllerPublishVolume(ctx, treq, info, handler)
	case *csi.ControllerUnpublishVolumeRequest:
		return i.controllerUnpublishVolume(ctx, treq, info, handler)
	case *csi.CreateVolumeRequest:
		return i.createVolume(ctx, treq, info, handler)
	case *csi.DeleteVolumeRequest:
		return i.deleteVolume(ctx, treq, info, handler)
	case *csi.NodePublishVolumeRequest:
		return i.nodePublishVolume(ctx, treq, info, handler)
	case *csi.NodeUnpublishVolumeRequest:
		return i.nodeUnpublishVolume(ctx, treq, info, handler)
	}

	return handler(ctx, req)
}

func (i *idempotencyInterceptor) controllerPublishVolume(
	ctx context.Context,
	req *csi.ControllerPublishVolumeRequest,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (res interface{}, resErr error) {

	lock := i.lockWithID(req.VolumeId)
	if !lock.TryLock(i.opts.timeout) {
		return ErrControllerPublishVolume(
			csi.Error_ControllerPublishVolumeError_OPERATION_PENDING_FOR_VOLUME,
			""), nil
	}

	// At the end of this function check for a response error or if
	// the response itself contains an error. If either is true then
	// mark the current method as in error.
	//
	// If neither is true then check to see if the method has been
	// marked in error in the past and remove that mark to reclaim
	// memory.
	defer func() {
		if resErr != nil ||
			res.(*csi.ControllerPublishVolumeResponse).GetError() != nil {
			lock.methodInErr[info.FullMethod] = struct{}{}
		} else if _, ok := lock.methodInErr[info.FullMethod]; ok {
			delete(lock.methodInErr, info.FullMethod)
		}
	}()
	defer lock.Unlock()

	// If the method has been marked in error then it means a previous
	// call to this function returned an error. In these cases a
	// subsequent call should bypass idempotency.
	if _, ok := lock.methodInErr[info.FullMethod]; ok {
		return handler(ctx, req)
	}

	// If configured to do so, check to see if the volume exists and
	// return an error if it does not.
	if i.opts.requireVolume {
		volInfo, err := i.p.GetVolumeInfo(ctx, req.VolumeId, "")
		if err != nil {
			return nil, err
		}
		if volInfo == nil {
			return ErrControllerPublishVolume(
				csi.Error_ControllerPublishVolumeError_VOLUME_DOES_NOT_EXIST,
				""), nil
		}
	}

	pubInfo, err := i.p.IsControllerPublished(ctx, req.VolumeId, req.NodeId)
	if err != nil {
		return nil, err
	}
	if pubInfo != nil {
		log.WithField("volumeID", req.VolumeId).Info(
			"idempotent controller publish")
		return &csi.ControllerPublishVolumeResponse{
			Reply: &csi.ControllerPublishVolumeResponse_Result_{
				Result: &csi.ControllerPublishVolumeResponse_Result{
					PublishVolumeInfo: pubInfo,
				},
			},
		}, nil
	}

	return handler(ctx, req)
}

func (i *idempotencyInterceptor) controllerUnpublishVolume(
	ctx context.Context,
	req *csi.ControllerUnpublishVolumeRequest,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (res interface{}, resErr error) {

	lock := i.lockWithID(req.VolumeId)
	if !lock.TryLock(i.opts.timeout) {
		return ErrControllerUnpublishVolume(
			csi.Error_ControllerUnpublishVolumeError_OPERATION_PENDING_FOR_VOLUME,
			""), nil
	}

	// At the end of this function check for a response error or if
	// the response itself contains an error. If either is true then
	// mark the current method as in error.
	//
	// If neither is true then check to see if the method has been
	// marked in error in the past and remove that mark to reclaim
	// memory.
	defer func() {
		if resErr != nil ||
			res.(*csi.ControllerUnpublishVolumeResponse).GetError() != nil {
			lock.methodInErr[info.FullMethod] = struct{}{}
		} else if _, ok := lock.methodInErr[info.FullMethod]; ok {
			delete(lock.methodInErr, info.FullMethod)
		}
	}()
	defer lock.Unlock()

	// If the method has been marked in error then it means a previous
	// call to this function returned an error. In these cases a
	// subsequent call should bypass idempotency.
	if _, ok := lock.methodInErr[info.FullMethod]; ok {
		return handler(ctx, req)
	}

	// If configured to do so, check to see if the volume exists and
	// return an error if it does not.
	if i.opts.requireVolume {
		volInfo, err := i.p.GetVolumeInfo(ctx, req.VolumeId, "")
		if err != nil {
			return nil, err
		}
		if volInfo == nil {
			return ErrControllerUnpublishVolume(
				csi.Error_ControllerUnpublishVolumeError_VOLUME_DOES_NOT_EXIST,
				""), nil
		}
	}

	pubInfo, err := i.p.IsControllerPublished(ctx, req.VolumeId, req.NodeId)
	if err != nil {
		return nil, err
	}
	if pubInfo == nil {
		log.WithField("volumeID", req.VolumeId).Info(
			"idempotent controller unpublish")
		return &csi.ControllerUnpublishVolumeResponse{
			Reply: &csi.ControllerUnpublishVolumeResponse_Result_{
				Result: &csi.ControllerUnpublishVolumeResponse_Result{},
			},
		}, nil
	}

	return handler(ctx, req)
}

func (i *idempotencyInterceptor) createVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (res interface{}, resErr error) {

	// First attempt to lock the volume by the provided name. If no lock
	// can be obtained then exit with the appropriate error.
	nameLock := i.lockWithName(req.Name)
	if !nameLock.TryLock(i.opts.timeout) {
		return ErrCreateVolume(
			csi.Error_CreateVolumeError_OPERATION_PENDING_FOR_VOLUME,
			""), nil
	}

	// At the end of this function check for a response error or if
	// the response itself contains an error. If either is true then
	// mark the current method as in error.
	//
	// If neither is true then check to see if the method has been
	// marked in error in the past and remove that mark to reclaim
	// memory.
	defer func() {
		if resErr != nil ||
			res.(*csi.CreateVolumeResponse).GetError() != nil {

			// Check to see if the error code is OPERATION_PENDING_FOR_VOLUME.
			// If it is then do not mark this method in error.
			terr := res.(*csi.CreateVolumeResponse).GetError()
			if terr, ok := terr.Value.(*csi.Error_CreateVolumeError_); ok &&
				terr.CreateVolumeError != nil &&
				terr.CreateVolumeError.ErrorCode ==
					csi.Error_CreateVolumeError_OPERATION_PENDING_FOR_VOLUME {
				return
			}
			nameLock.methodInErr[info.FullMethod] = struct{}{}
		} else if _, ok := nameLock.methodInErr[info.FullMethod]; ok {
			delete(nameLock.methodInErr, info.FullMethod)
		}
	}()
	defer nameLock.Unlock()

	// If the method has been marked in error then it means a previous
	// call to this function returned an error. In these cases a
	// subsequent call should bypass idempotency.
	if _, ok := nameLock.methodInErr[info.FullMethod]; ok {
		log.WithField("volumeName", req.Name).Warn("creating volume: nameInErr")
		return handler(ctx, req)
	}

	// Next, attempt to get the volume info based on the name.
	volInfo, err := i.p.GetVolumeInfo(ctx, "", req.Name)
	if err != nil {
		return nil, err
	}

	// If the volInfo is nil then it means the volume does not exist.
	// Return early, passing control to the next handler in the chain.
	if volInfo == nil {
		log.WithField("volumeName", req.Name).Warn("creating volume")
		return handler(ctx, req)
	}

	// If the volInfo is not nil it means the volume already exists.
	// The volume info contains the volume's ID. Use that to obtain a
	// volume ID-based lock for the volume.
	idLock := i.lockWithID(volInfo.Id)
	if !idLock.TryLock(i.opts.timeout) {
		return ErrCreateVolume(
			csi.Error_CreateVolumeError_OPERATION_PENDING_FOR_VOLUME,
			""), nil
	}

	// At the end of this function check for a response error or if
	// the response itself contains an error. If either is true then
	// mark the current method as in error.
	//
	// If neither is true then check to see if the method has been
	// marked in error in the past and remove that mark to reclaim
	// memory.
	defer func() {
		if resErr != nil ||
			res.(*csi.CreateVolumeResponse).GetError() != nil {
			idLock.methodInErr[info.FullMethod] = struct{}{}
		} else if _, ok := idLock.methodInErr[info.FullMethod]; ok {
			delete(idLock.methodInErr, info.FullMethod)
		}
	}()
	defer idLock.Unlock()

	// If the method has been marked in error then it means a previous
	// call to this function returned an error. In these cases a
	// subsequent call should bypass idempotency.
	if _, ok := idLock.methodInErr[info.FullMethod]; ok {
		log.WithField("volumeName", req.Name).Warn("creating volume: idInErr")
		return handler(ctx, req)
	}

	// The ID lock has been obtained. Once again call GetVolumeInfo,
	// this time with the volume ID, now that the ID lock is held.
	// This ensures the volume still exists since it could have been
	// removed in the time it took to obtain the ID lock.
	volInfo, err = i.p.GetVolumeInfo(ctx, volInfo.Id, "")
	if err != nil {
		return nil, err
	}

	// If the volume info is nil it means the volume was removed in
	// the time it took to obtain the lock ID. Return early, passing
	// control to the next handler in the chain.
	if volInfo == nil {
		log.WithField("volumeName", req.Name).Warn("creating volume: 2")
		return handler(ctx, req)
	}

	// If the volume info still exists then it means the volume
	// exists! Go ahead and return the volume info and note this
	// as an idempotent create call.
	log.WithFields(map[string]interface{}{
		"volumeID":   volInfo.Id,
		"volumeName": req.Name}).Info("idempotent create")
	return &csi.CreateVolumeResponse{
		Reply: &csi.CreateVolumeResponse_Result_{
			Result: &csi.CreateVolumeResponse_Result{
				VolumeInfo: volInfo,
			},
		},
	}, nil
}

func (i *idempotencyInterceptor) deleteVolume(
	ctx context.Context,
	req *csi.DeleteVolumeRequest,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (res interface{}, resErr error) {

	lock := i.lockWithID(req.VolumeId)
	if !lock.TryLock(i.opts.timeout) {
		return ErrDeleteVolume(
			csi.Error_DeleteVolumeError_OPERATION_PENDING_FOR_VOLUME,
			""), nil
	}

	// At the end of this function check for a response error or if
	// the response itself contains an error. If either is true then
	// mark the current method as in error.
	//
	// If neither is true then check to see if the method has been
	// marked in error in the past and remove that mark to reclaim
	// memory.
	defer func() {
		if resErr != nil ||
			res.(*csi.DeleteVolumeResponse).GetError() != nil {
			lock.methodInErr[info.FullMethod] = struct{}{}
		} else if _, ok := lock.methodInErr[info.FullMethod]; ok {
			delete(lock.methodInErr, info.FullMethod)
		}
	}()
	defer lock.Unlock()

	// If the method has been marked in error then it means a previous
	// call to this function returned an error. In these cases a
	// subsequent call should bypass idempotency.
	if _, ok := lock.methodInErr[info.FullMethod]; ok {
		return handler(ctx, req)
	}

	// If configured to do so, check to see if the volume exists and
	// return an error if it does not.
	var volExists bool
	if i.opts.requireVolume {
		volInfo, err := i.p.GetVolumeInfo(ctx, req.VolumeId, "")
		if err != nil {
			return nil, err
		}
		if volInfo == nil {
			return ErrDeleteVolume(
				csi.Error_DeleteVolumeError_VOLUME_DOES_NOT_EXIST,
				""), nil
		}
		volExists = true
	}

	// Check to see if the volume exists if that has not yet been
	// verified above.
	if !volExists {
		volInfo, err := i.p.GetVolumeInfo(ctx, req.VolumeId, "")
		if err != nil {
			return nil, err
		}
		volExists = volInfo != nil
	}

	// Indicate an idempotent delete operation if the volume does not exist.
	if !volExists {
		log.WithField("volumeID", req.VolumeId).Info("idempotent delete")
		return &csi.DeleteVolumeResponse{
			Reply: &csi.DeleteVolumeResponse_Result_{
				Result: &csi.DeleteVolumeResponse_Result{},
			},
		}, nil
	}

	return handler(ctx, req)
}

func (i *idempotencyInterceptor) nodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (res interface{}, resErr error) {

	lock := i.lockWithID(req.VolumeId)
	if !lock.TryLock(i.opts.timeout) {
		return ErrNodePublishVolume(
			csi.Error_NodePublishVolumeError_OPERATION_PENDING_FOR_VOLUME,
			""), nil
	}

	// At the end of this function check for a response error or if
	// the response itself contains an error. If either is true then
	// mark the current method as in error.
	//
	// If neither is true then check to see if the method has been
	// marked in error in the past and remove that mark to reclaim
	// memory.
	defer func() {
		if resErr != nil ||
			res.(*csi.NodePublishVolumeResponse).GetError() != nil {
			lock.methodInErr[info.FullMethod] = struct{}{}
		} else if _, ok := lock.methodInErr[info.FullMethod]; ok {
			delete(lock.methodInErr, info.FullMethod)
		}
	}()
	defer lock.Unlock()

	// If the method has been marked in error then it means a previous
	// call to this function returned an error. In these cases a
	// subsequent call should bypass idempotency.
	if _, ok := lock.methodInErr[info.FullMethod]; ok {
		return handler(ctx, req)
	}

	// If configured to do so, check to see if the volume exists and
	// return an error if it does not.
	if i.opts.requireVolume {
		volInfo, err := i.p.GetVolumeInfo(ctx, req.VolumeId, "")
		if err != nil {
			return nil, err
		}
		if volInfo == nil {
			return ErrNodePublishVolume(
				csi.Error_NodePublishVolumeError_VOLUME_DOES_NOT_EXIST,
				""), nil
		}
	}

	ok, err := i.p.IsNodePublished(
		ctx, req.VolumeId, req.PublishVolumeInfo, req.TargetPath)
	if err != nil {
		return nil, err
	}
	if ok {
		log.WithField("volumeId", req.VolumeId).Info("idempotent node publish")
		return &csi.NodePublishVolumeResponse{
			Reply: &csi.NodePublishVolumeResponse_Result_{
				Result: &csi.NodePublishVolumeResponse_Result{},
			},
		}, nil
	}

	return handler(ctx, req)
}

func (i *idempotencyInterceptor) nodeUnpublishVolume(
	ctx context.Context,
	req *csi.NodeUnpublishVolumeRequest,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (res interface{}, resErr error) {

	lock := i.lockWithID(req.VolumeId)
	if !lock.TryLock(i.opts.timeout) {
		return ErrNodeUnpublishVolume(
			csi.Error_NodeUnpublishVolumeError_OPERATION_PENDING_FOR_VOLUME,
			""), nil
	}

	// At the end of this function check for a response error or if
	// the response itself contains an error. If either is true then
	// mark the current method as in error.
	//
	// If neither is true then check to see if the method has been
	// marked in error in the past and remove that mark to reclaim
	// memory.
	defer func() {
		if resErr != nil ||
			res.(*csi.NodeUnpublishVolumeResponse).GetError() != nil {
			lock.methodInErr[info.FullMethod] = struct{}{}
		} else if _, ok := lock.methodInErr[info.FullMethod]; ok {
			delete(lock.methodInErr, info.FullMethod)
		}
	}()
	defer lock.Unlock()

	// If the method has been marked in error then it means a previous
	// call to this function returned an error. In these cases a
	// subsequent call should bypass idempotency.
	if _, ok := lock.methodInErr[info.FullMethod]; ok {
		return handler(ctx, req)
	}

	// If configured to do so, check to see if the volume exists and
	// return an error if it does not.
	if i.opts.requireVolume {
		volInfo, err := i.p.GetVolumeInfo(ctx, req.VolumeId, "")
		if err != nil {
			return nil, err
		}
		if volInfo == nil {
			return ErrNodeUnpublishVolume(
				csi.Error_NodeUnpublishVolumeError_VOLUME_DOES_NOT_EXIST,
				""), nil
		}
	}

	ok, err := i.p.IsNodePublished(ctx, req.VolumeId, nil, req.TargetPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		log.WithField("volumeId", req.VolumeId).Info(
			"idempotent node unpublish")
		return &csi.NodeUnpublishVolumeResponse{
			Reply: &csi.NodeUnpublishVolumeResponse_Result_{
				Result: &csi.NodeUnpublishVolumeResponse_Result{},
			},
		}, nil
	}

	return handler(ctx, req)
}
