package service

import (
	"fmt"
	"math"
	"path"
	"strconv"

	"golang.org/x/net/context"

	"github.com/thecodeteam/gocsi"
	"github.com/thecodeteam/gocsi/csi"
)

func (s *service) CreateVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest) (
	*csi.CreateVolumeResponse, error) {

	// If no capacity is specified then use 100GiB
	capacity := gib100
	if cr := req.CapacityRange; cr != nil {
		if rb := cr.RequiredBytes; rb > 0 {
			capacity = rb
		}
		if lb := cr.LimitBytes; lb > 0 {
			capacity = lb
		}
	}

	// Create the volume and add it to the service's in-mem volume slice.
	v := s.newVolume(req.Name, capacity)
	s.volsRWL.Lock()
	defer s.volsRWL.Unlock()
	s.vols = append(s.vols, v)

	return &csi.CreateVolumeResponse{
		Reply: &csi.CreateVolumeResponse_Result_{
			Result: &csi.CreateVolumeResponse_Result{
				VolumeInfo: &v,
			},
		},
	}, nil
}

func (s *service) DeleteVolume(
	ctx context.Context,
	req *csi.DeleteVolumeRequest) (
	*csi.DeleteVolumeResponse, error) {

	func() {
		s.volsRWL.Lock()
		defer s.volsRWL.Unlock()
		i, _ := s.findVolNoLock("id", req.VolumeId)

		// This delete logic preserves order and prevents potential memory
		// leaks. The slice's elements may not be pointers, but the structs
		// themselves have fields that are.
		copy(s.vols[i:], s.vols[i+1:])
		s.vols[len(s.vols)-1] = csi.VolumeInfo{}
		s.vols = s.vols[:len(s.vols)-1]
	}()

	return &csi.DeleteVolumeResponse{
		Reply: &csi.DeleteVolumeResponse_Result_{
			Result: &csi.DeleteVolumeResponse_Result{},
		},
	}, nil
}

func (s *service) ControllerPublishVolume(
	ctx context.Context,
	req *csi.ControllerPublishVolumeRequest) (
	*csi.ControllerPublishVolumeResponse, error) {

	// devPathKey is the key in the volume's attributes that is set to a
	// mock device path if the volume has been published by the controller
	// to the specified node.
	devPathKey := path.Join(req.NodeId, "dev")

	s.volsRWL.Lock()
	defer s.volsRWL.Unlock()
	i, v := s.findVolNoLock("id", req.VolumeId)

	// Publish the volume.
	v.Attributes[devPathKey] = "/dev/mock"
	s.vols[i] = v

	return &csi.ControllerPublishVolumeResponse{
		Reply: &csi.ControllerPublishVolumeResponse_Result_{
			Result: &csi.ControllerPublishVolumeResponse_Result{
				PublishVolumeInfo: map[string]string{
					"device": v.Attributes[devPathKey],
				},
			},
		},
	}, nil
}

func (s *service) ControllerUnpublishVolume(
	ctx context.Context,
	req *csi.ControllerUnpublishVolumeRequest) (
	*csi.ControllerUnpublishVolumeResponse, error) {

	// devPathKey is the key in the volume's attributes that is set to a
	// mock device path if the volume has been published by the controller
	// to the specified node.
	devPathKey := path.Join(req.NodeId, "dev")

	s.volsRWL.Lock()
	defer s.volsRWL.Unlock()
	i, v := s.findVolNoLock("id", req.VolumeId)

	// Unpublish the volume.
	delete(v.Attributes, devPathKey)
	s.vols[i] = v

	return &csi.ControllerUnpublishVolumeResponse{
		Reply: &csi.ControllerUnpublishVolumeResponse_Result_{
			Result: &csi.ControllerUnpublishVolumeResponse_Result{},
		},
	}, nil
}

func (s *service) ValidateVolumeCapabilities(
	ctx context.Context,
	req *csi.ValidateVolumeCapabilitiesRequest) (
	*csi.ValidateVolumeCapabilitiesResponse, error) {

	return &csi.ValidateVolumeCapabilitiesResponse{
		Reply: &csi.ValidateVolumeCapabilitiesResponse_Result_{
			Result: &csi.ValidateVolumeCapabilitiesResponse_Result{
				Supported: true,
			},
		},
	}, nil
}

func (s *service) ListVolumes(
	ctx context.Context,
	req *csi.ListVolumesRequest) (
	*csi.ListVolumesResponse, error) {

	// Copy the mock volumes into a new slice in order to avoid
	// locking the service's volume slice for the duration of the
	// ListVolumes RPC.
	var vols []csi.VolumeInfo
	func() {
		s.volsRWL.RLock()
		defer s.volsRWL.RUnlock()
		vols = make([]csi.VolumeInfo, len(s.vols))
		copy(vols, s.vols)
	}()

	var (
		ulenVols      = uint32(len(vols))
		maxEntries    = req.MaxEntries
		startingToken uint32
	)

	if v := req.StartingToken; v != "" {
		i, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return gocsi.ErrListVolumes(0, fmt.Sprintf(
				"startingToken=%d !< uint32=%d",
				startingToken, math.MaxUint32)), nil
		}
		startingToken = uint32(i)
	}

	if startingToken > ulenVols {
		return gocsi.ErrListVolumes(0, fmt.Sprintf(
			"startingToken=%d > len(vols)=%d",
			startingToken, ulenVols)), nil
	}

	// Discern the number of remaining entries.
	rem := ulenVols - startingToken

	// If maxEntries is 0 or greater than the number of remaining entries then
	// set maxEntries to the number of remaining entries.
	if maxEntries == 0 || maxEntries > rem {
		maxEntries = rem
	}

	var (
		i       int
		j       = startingToken
		entries = make(
			[]*csi.ListVolumesResponse_Result_Entry,
			maxEntries)
	)

	for i = 0; i < len(entries); i++ {
		entries[i] = &csi.ListVolumesResponse_Result_Entry{
			VolumeInfo: &vols[j],
		}
		j++
	}

	var nextToken string
	if n := startingToken + uint32(i); n < ulenVols {
		nextToken = fmt.Sprintf("%d", n)
	}

	return &csi.ListVolumesResponse{
		Reply: &csi.ListVolumesResponse_Result_{
			Result: &csi.ListVolumesResponse_Result{
				Entries:   entries,
				NextToken: nextToken,
			},
		},
	}, nil
}

func (s *service) GetCapacity(
	ctx context.Context,
	req *csi.GetCapacityRequest) (
	*csi.GetCapacityResponse, error) {

	return &csi.GetCapacityResponse{
		Reply: &csi.GetCapacityResponse_Result_{
			Result: &csi.GetCapacityResponse_Result{
				AvailableCapacity: tib100,
			},
		},
	}, nil
}

func (s *service) ControllerGetCapabilities(
	ctx context.Context,
	req *csi.ControllerGetCapabilitiesRequest) (
	*csi.ControllerGetCapabilitiesResponse, error) {

	return &csi.ControllerGetCapabilitiesResponse{
		Reply: &csi.ControllerGetCapabilitiesResponse_Result_{
			Result: &csi.ControllerGetCapabilitiesResponse_Result{
				Capabilities: []*csi.ControllerServiceCapability{
					&csi.ControllerServiceCapability{
						Type: &csi.ControllerServiceCapability_Rpc{
							Rpc: &csi.ControllerServiceCapability_RPC{
								Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
							},
						},
					},
					&csi.ControllerServiceCapability{
						Type: &csi.ControllerServiceCapability_Rpc{
							Rpc: &csi.ControllerServiceCapability_RPC{
								Type: csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
							},
						},
					},
					&csi.ControllerServiceCapability{
						Type: &csi.ControllerServiceCapability_Rpc{
							Rpc: &csi.ControllerServiceCapability_RPC{
								Type: csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
							},
						},
					},
					&csi.ControllerServiceCapability{
						Type: &csi.ControllerServiceCapability_Rpc{
							Rpc: &csi.ControllerServiceCapability_RPC{
								Type: csi.ControllerServiceCapability_RPC_GET_CAPACITY,
							},
						},
					},
				},
			},
		},
	}, nil
}

func (s *service) ControllerProbe(
	ctx context.Context,
	req *csi.ControllerProbeRequest) (
	*csi.ControllerProbeResponse, error) {

	return &csi.ControllerProbeResponse{
		Reply: &csi.ControllerProbeResponse_Result_{
			Result: &csi.ControllerProbeResponse_Result{},
		},
	}, nil
}
