package ebssurrogate

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	multierror "github.com/hashicorp/go-multierror"
	awscommon "github.com/hashicorp/packer/builder/amazon/common"
	"github.com/hashicorp/packer/helper/multistep"
	"github.com/hashicorp/packer/packer"
)

// StepSnapshotVolumes creates snapshots of the created volumes.
//
// Produces:
//   snapshot_ids map[string]string - IDs of the created snapshots
type StepSnapshotVolumes struct {
	LaunchDevices   []*ec2.BlockDeviceMapping
	snapshotIds     map[string]string
	snapshotMutex   sync.Mutex
	SnapshotOmitMap map[string]bool
}

func (s *StepSnapshotVolumes) snapshotVolume(ctx context.Context, deviceName string, state multistep.StateBag) error {
	ec2conn := state.Get("ec2").(*ec2.EC2)
	ui := state.Get("ui").(packer.Ui)
	instance := state.Get("instance").(*ec2.Instance)

	var volumeId string
	for _, volume := range instance.BlockDeviceMappings {
		if *volume.DeviceName == deviceName {
			volumeId = *volume.Ebs.VolumeId
		}
	}
	if volumeId == "" {
		return fmt.Errorf("Volume ID for device %s not found", deviceName)
	}

	ui.Say(fmt.Sprintf("Creating snapshot of EBS Volume %s...", volumeId))
	description := fmt.Sprintf("Packer: %s", time.Now().String())

	createSnapResp, err := ec2conn.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId:    &volumeId,
		Description: &description,
	})
	if err != nil {
		return err
	}

	// Set the snapshot ID so we can delete it later
	s.snapshotMutex.Lock()
	s.snapshotIds[deviceName] = *createSnapResp.SnapshotId
	s.snapshotMutex.Unlock()

	// Wait for snapshot to be created
	err = awscommon.WaitUntilSnapshotDone(ctx, ec2conn, *createSnapResp.SnapshotId)
	return err
}

func (s *StepSnapshotVolumes) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ui := state.Get("ui").(packer.Ui)

	s.snapshotIds = map[string]string{}

	var wg sync.WaitGroup
	var errs *multierror.Error
	for _, device := range s.LaunchDevices {
		// Skip devices we've flagged for omission
		omit, ok := s.SnapshotOmitMap[*device.DeviceName]
		if ok && omit {
			continue
		}

		wg.Add(1)
		go func(device *ec2.BlockDeviceMapping) {
			defer wg.Done()
			if err := s.snapshotVolume(ctx, *device.DeviceName, state); err != nil {
				errs = multierror.Append(errs, err)
			}
		}(device)
	}

	wg.Wait()

	if errs != nil {
		state.Put("error", errs)
		ui.Error(errs.Error())
		return multistep.ActionHalt
	}

	state.Put("snapshot_ids", s.snapshotIds)
	return multistep.ActionContinue
}

func (s *StepSnapshotVolumes) Cleanup(state multistep.StateBag) {
	if len(s.snapshotIds) == 0 {
		return
	}

	_, cancelled := state.GetOk(multistep.StateCancelled)
	_, halted := state.GetOk(multistep.StateHalted)

	if cancelled || halted {
		ec2conn := state.Get("ec2").(*ec2.EC2)
		ui := state.Get("ui").(packer.Ui)
		ui.Say("Removing snapshots since we cancelled or halted...")
		s.snapshotMutex.Lock()
		for _, snapshotId := range s.snapshotIds {
			_, err := ec2conn.DeleteSnapshot(&ec2.DeleteSnapshotInput{SnapshotId: &snapshotId})
			if err != nil {
				ui.Error(fmt.Sprintf("Error: %s", err))
			}
		}
		s.snapshotMutex.Unlock()
	}
}
