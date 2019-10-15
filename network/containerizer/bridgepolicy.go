// Copyright 2017 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package containerizer

import (
	"fmt"
	"hash/crc32"
	"sort"
	"strings"

	"github.com/juju/collections/set"
	"github.com/juju/errors"
	"github.com/juju/loggo"

	"github.com/juju/juju/core/instance"
	corenetwork "github.com/juju/juju/core/network"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/network"
	"github.com/juju/juju/state"
)

var logger = loggo.GetLogger("juju.network.containerizer")

var skippedDeviceNames = set.NewStrings(
	network.DefaultLXCBridge,
	network.DefaultLXDBridge,
	network.DefaultKVMBridge,
)

// BridgePolicy defines functionality that helps us create and define bridges
// for guests inside of a host machine, along with the creation of network
// devices on those bridges for the containers to use.
type BridgePolicy struct {
	// spaces is a cache of the model's spaces.
	spaces Spaces

	// netBondReconfigureDelay is how much of a delay to inject if we see that
	// one of the devices being bridged is a BondDevice. This exists because of
	// https://bugs.launchpad.net/juju/+bug/1657579
	netBondReconfigureDelay int

	// containerNetworkingMethod defines the way containers are networked.
	// It's one of:
	//  - fan
	//  - provider
	//  - local
	containerNetworkingMethod string
}

// NewBridgePolicy returns a new BridgePolicy for the input environ config
// getter and state indirection.
func NewBridgePolicy(cfgGetter environs.ConfigGetter, st SpaceBacking) (*BridgePolicy, error) {
	cfg := cfgGetter.Config()

	spaces, err := NewSpaces(st)
	if err != nil {
		return nil, errors.Annotate(err, "creating spaces cache")
	}

	return &BridgePolicy{
		spaces:                    spaces,
		netBondReconfigureDelay:   cfg.NetBondReconfigureDelay(),
		containerNetworkingMethod: cfg.ContainerNetworkingMethod(),
	}, nil
}

// FindMissingBridgesForContainer looks at the spaces that the container should
// have access to, and returns any host devices need to be bridged for use as
// the container network.
// This will return an Error if the container requires a space that the host
// machine cannot provide.
func (p *BridgePolicy) FindMissingBridgesForContainer(
	host Machine, guest Container,
) ([]network.DeviceToBridge, int, error) {
	guestSpaces, devicesPerSpace, err := p.findSpacesAndDevicesForContainer(host, guest)
	if err != nil {
		return nil, 0, errors.Trace(err)
	}
	logger.Debugf("FindMissingBridgesForContainer(%q) spaces %s devices %v",
		guest.Id(), guestSpaces.String(), formatDeviceMap(devicesPerSpace))

	spacesFound := set.NewStrings()
	fanSpacesFound := set.NewStrings()
	for spaceID, devices := range devicesPerSpace {
		for _, device := range devices {
			if device.Type() == corenetwork.BridgeDevice {
				if p.containerNetworkingMethod != "local" && skippedDeviceNames.Contains(device.Name()) {
					continue
				}
				if strings.HasPrefix(device.Name(), "fan-") {
					fanSpacesFound.Add(spaceID)
				} else {
					spacesFound.Add(spaceID)
				}
			}
		}
	}

	// TODO (manadart 2019-09-27): This is ugly, but once everything is
	// consistently reasoning about spaces in terms of IDs, we should implement
	// this kind of diffing on SpaceInfos.
	// network.QuoteSpaceSet can be removed at that time too.
	guestSpaceSet := set.NewStrings(guestSpaces.IDs()...)
	notFound := guestSpaceSet.Difference(spacesFound)
	fanNotFound := guestSpaceSet.Difference(fanSpacesFound)

	if p.containerNetworkingMethod == "fan" {
		if fanNotFound.IsEmpty() {
			// Nothing to do; just return success.
			return nil, 0, nil
		}
		return nil, 0, errors.Errorf("host machine %q has no available FAN devices in space(s) %s",
			host.Id(), network.QuoteSpaceSet(fanNotFound))
	}

	if notFound.IsEmpty() {
		// Nothing to do; just return success.
		return nil, 0, nil
	}

	hostDeviceNamesToBridge := make([]string, 0)
	reconfigureDelay := 0
	hostDeviceByName := make(map[string]LinkLayerDevice, 0)
	for _, spaceID := range notFound.Values() {
		hostDeviceNames := make([]string, 0)
		for _, hostDevice := range devicesPerSpace[spaceID] {
			possible, err := possibleBridgeTarget(hostDevice)
			if err != nil {
				return nil, 0, err
			}
			if !possible {
				continue
			}
			hostDeviceNames = append(hostDeviceNames, hostDevice.Name())
			hostDeviceByName[hostDevice.Name()] = hostDevice
			spacesFound.Add(spaceID)
		}
		if len(hostDeviceNames) > 0 {
			if spaceID == corenetwork.DefaultSpaceId {
				// When we are bridging unknown space devices, we bridge all
				// of them. Both because this is a fallback, and because we
				// don't know what the exact spaces are going to be.
				for _, deviceName := range hostDeviceNames {
					hostDeviceNamesToBridge = append(hostDeviceNamesToBridge, deviceName)
					if hostDeviceByName[deviceName].Type() == corenetwork.BondDevice {
						if reconfigureDelay < p.netBondReconfigureDelay {
							reconfigureDelay = p.netBondReconfigureDelay
						}
					}
				}
			} else {
				// This should already be sorted from
				// LinkLayerDevicesForSpaces but sorting to be sure we stably
				// pick the host device
				hostDeviceNames = network.NaturallySortDeviceNames(hostDeviceNames...)
				hostDeviceNamesToBridge = append(hostDeviceNamesToBridge, hostDeviceNames[0])
				if hostDeviceByName[hostDeviceNames[0]].Type() == corenetwork.BondDevice {
					if reconfigureDelay < p.netBondReconfigureDelay {
						reconfigureDelay = p.netBondReconfigureDelay
					}
				}
			}
		}
	}
	notFound = notFound.Difference(spacesFound)
	if !notFound.IsEmpty() {
		hostSpaces, err := host.AllSpaces()
		if err != nil {
			// log it, but we're returning another error right now
			logger.Warningf("got error looking for spaces for host machine %q: %v",
				host.Id(), err)
		}
		logger.Warningf("container %q wants spaces %s, but host machine %q has %s missing %s",
			guest.Id(), network.QuoteSpaceSet(guestSpaceSet),
			host.Id(), network.QuoteSpaceSet(hostSpaces), network.QuoteSpaceSet(notFound))
		return nil, 0, errors.Errorf("host machine %q has no available device in space(s) %s",
			host.Id(), network.QuoteSpaceSet(notFound))
	}

	hostToBridge := make([]network.DeviceToBridge, 0, len(hostDeviceNamesToBridge))
	for _, hostName := range network.NaturallySortDeviceNames(hostDeviceNamesToBridge...) {
		hostToBridge = append(hostToBridge, network.DeviceToBridge{
			DeviceName: hostName,
			BridgeName: BridgeNameForDevice(hostName),
			MACAddress: hostDeviceByName[hostName].MACAddress(),
		})
	}
	return hostToBridge, reconfigureDelay, nil
}

// findSpacesAndDevicesForContainer looks up what spaces the container wants
// to be in, and what spaces the host machine is already in, and tries to
// find the devices on the host that are useful for the container.
func (p *BridgePolicy) findSpacesAndDevicesForContainer(
	host Machine, guest Container,
) (corenetwork.SpaceInfos, map[string][]LinkLayerDevice, error) {
	containerSpaces, err := p.determineContainerSpaces(host, guest)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	devicesPerSpace, err := linkLayerDevicesForSpaces(host, containerSpaces)
	if err != nil {
		logger.Errorf("findSpacesAndDevicesForContainer(%q) got error looking for host spaces: %v",
			guest.Id(), err)
		return nil, nil, errors.Trace(err)
	}
	return containerSpaces, devicesPerSpace, nil
}

// linkLayerDevicesForSpaces takes a list of SpaceInfos, and returns
// the devices on this machine that are in those spaces that we feel
// would be useful for containers to know about.  (eg, if there is a
// host device that has been bridged, we return the bridge, rather
// than the underlying device, but if we have only the host device,
// we return that.)
// Note that devices like 'lxdbr0' that are bridges that might not be
// externally accessible may be returned if the default space is
// listed as one of the desired spaces.
func linkLayerDevicesForSpaces(host Machine, spaces corenetwork.SpaceInfos) (map[string][]LinkLayerDevice, error) {
	deviceByName, err := linkLayerDevicesByName(host)
	if err != nil {
		return nil, errors.Trace(err)
	}
	processedDeviceNames := set.NewStrings()
	spaceToDevices := make(map[string]map[string]LinkLayerDevice, 0)

	// First pass, iterate the addresses, lookup the associated spaces, and
	// gather the devices.
	addresses, err := host.AllAddresses()
	if err != nil {
		return nil, errors.Trace(err)
	}
	for _, addr := range addresses {
		device, ok := deviceByName[addr.DeviceName()]
		if !ok {
			return nil, errors.Errorf("address %v for machine %q refers to a missing device %q",
				addr, host.Id(), addr.DeviceName())
		}
		processedDeviceNames.Add(device.Name())

		// We do not care about loopback devices.
		if device.Type() == corenetwork.LoopbackDevice {
			continue
		}

		spaceID := corenetwork.DefaultSpaceId

		subnet, err := addr.Subnet()
		if err != nil {
			if !errors.IsNotFound(err) {
				// We don't understand the error, so error out for now
				return nil, errors.Trace(err)
			}
		} else {
			spaceID = subnet.SpaceID()
		}
		spaceToDevices = includeDevice(spaceToDevices, spaceID, device)
	}

	// Second pass, grab any devices we may have missed. For now, any device without an
	// address must be in the default space.
	for devName, device := range deviceByName {
		if processedDeviceNames.Contains(devName) {
			continue
		}
		// Loopback devices are not considered part of the empty space.
		// Also, devices that are attached to another device also aren't
		// considered to be in the unknown space.
		if device.Type() == corenetwork.LoopbackDevice || device.ParentName() != "" {
			continue
		}
		spaceToDevices = includeDevice(spaceToDevices, corenetwork.DefaultSpaceId, device)
	}

	requestedSpaces := set.NewStrings(spaces.IDs()...)
	result := make(map[string][]LinkLayerDevice, len(spaceToDevices))
	for spaceID, deviceMap := range spaceToDevices {
		if !requestedSpaces.Contains(spaceID) {
			continue
		}
		result[spaceID] = deviceMapToSortedList(deviceMap)
	}
	return result, nil
}

func linkLayerDevicesByName(host Machine) (map[string]LinkLayerDevice, error) {
	devices, err := host.AllLinkLayerDevices()
	if err != nil {
		return nil, errors.Trace(err)
	}
	deviceByName := make(map[string]LinkLayerDevice, len(devices))
	for _, dev := range devices {
		deviceByName[dev.Name()] = dev
	}
	return deviceByName, nil
}

func includeDevice(spaceToDevices map[string]map[string]LinkLayerDevice, spaceID string, device LinkLayerDevice) map[string]map[string]LinkLayerDevice {
	spaceInfo, ok := spaceToDevices[spaceID]
	if !ok {
		spaceInfo = make(map[string]LinkLayerDevice)
		spaceToDevices[spaceID] = spaceInfo
	}
	spaceInfo[device.Name()] = device
	return spaceToDevices
}

// deviceMapToSortedList takes a map from device name to LinkLayerDevice
// object, and returns the list of LinkLayerDevice object using
// NaturallySortDeviceNames
func deviceMapToSortedList(deviceMap map[string]LinkLayerDevice) []LinkLayerDevice {
	names := make([]string, 0, len(deviceMap))
	for name := range deviceMap {
		// name must == device.Name()
		names = append(names, name)
	}
	sortedNames := network.NaturallySortDeviceNames(names...)
	result := make([]LinkLayerDevice, len(sortedNames))
	for i, name := range sortedNames {
		result[i] = deviceMap[name]
	}
	return result
}

// determineContainerSpaces tries to use the direct information about a
// container to find what spaces it should be in, and then falls back to what
// we know about the host machine.
func (p *BridgePolicy) determineContainerSpaces(
	host Machine, guest Container,
) (corenetwork.SpaceInfos, error) {
	// Gather any *positive* space constraints for the guest.
	cons, err := guest.Constraints()
	if err != nil {
		return nil, errors.Trace(err)
	}

	spaces := set.NewStrings()
	// Constraints have been left in space name form,
	// as they are human readable and can be changed.
	for _, spaceName := range cons.IncludeSpaces() {
		info, err := p.spaces.GetByName(spaceName)
		if err != nil {
			return nil, errors.Trace(err)
		}
		spaces.Add(info.ID)
	}

	// Gather any space bindings for application endpoints
	// that apply to units that the container will host.
	// TODO (manadart 2019-10-08): This is not necessary now that we convert
	// endpoint bindings into machine space constraints when placing units.
	// However it remains in case we fix that logic properly to do machine
	// creation and assignment in a single transaction.
	// See `state.AssignUnitWithPlacement`.
	units, err := guest.Units()
	if err != nil {
		return nil, errors.Trace(err)
	}
	bindings := set.NewStrings()
	for _, unit := range units {
		app, err := unit.Application()
		if err != nil {
			return nil, errors.Trace(err)
		}
		endpointBindings, err := app.EndpointBindings()
		if err != nil {
			return nil, errors.Trace(err)
		}
		for _, space := range endpointBindings {
			bindings.Add(space)
		}
	}

	logger.Tracef("machine %q found constraints %s and bindings %s",
		guest.Id(), network.QuoteSpaceSet(spaces), network.QuoteSpaceSet(bindings))

	spaces = spaces.Union(bindings)
	logger.Debugf("for container %q, found desired spaces: %s", guest.Id(), network.QuoteSpaceSet(spaces))

	if len(spaces) == 0 {
		// We have determined that the container doesn't have any useful
		// constraints set on it. So lets see if we can come up with
		// something useful.
		spaces, err = p.inferContainerSpaces(host, guest.Id())
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	spaceInfos := make(corenetwork.SpaceInfos, len(spaces))
	for i, space := range spaces.Values() {
		if spaceInfos[i], err = p.spaces.GetByID(space); err != nil {
			return nil, errors.Trace(err)
		}
	}
	return spaceInfos, nil
}

// inferContainerSpaces tries to find a valid space for the container to be
// on. This should only be used when the container itself doesn't have any
// valid constraints on what spaces it should be in.
// If containerNetworkingMethod is 'local' we fall back to the default space
// and use lxdbr0.
// If this machine is in a single space, then that space is used.
// Otherwise we return an error.  If this occurs, there is a problem,
// as a machine should ALWAYS be in a space.
func (p *BridgePolicy) inferContainerSpaces(host Machine, containerId string) (set.Strings, error) {
	if p.containerNetworkingMethod == "local" {
		return set.NewStrings(corenetwork.DefaultSpaceId), nil
	}
	hostSpaces, err := host.AllSpaces()
	if err != nil {
		return nil, errors.Trace(err)
	}
	logger.Debugf("container %q not qualified to a space, host machine %q is using spaces %s",
		containerId, host.Id(), network.QuoteSpaceSet(hostSpaces))
	// Note: if a machine can be in more than 1 space, this needs
	// updating with choice criteria.
	if len(hostSpaces) == 1 {
		return hostSpaces, nil
	}
	if len(hostSpaces) == 0 {
		logger.Debugf("container has no desired spaces, " +
			"and host has no known spaces, triggering fallback " +
			"to bridge all devices")
		return set.NewStrings(corenetwork.DefaultSpaceId), nil
	}
	return nil, errors.Errorf("no obvious space for container %q, host machine has spaces: %s",
		containerId, network.QuoteSpaceSet(hostSpaces))
}

func possibleBridgeTarget(dev LinkLayerDevice) (bool, error) {
	// LoopbackDevices can never be bridged
	if dev.Type() == corenetwork.LoopbackDevice || dev.Type() == corenetwork.BridgeDevice {
		return false, nil
	}
	// Devices that have no parent entry are direct host devices that can be
	// bridged.
	if dev.ParentName() == "" {
		return true, nil
	}
	// TODO(jam): 2016-12-22 This feels dirty, but it falls out of how we are
	// currently modeling VLAN objects.  see bug https://pad.lv/1652049
	if dev.Type() != corenetwork.VLAN8021QDevice {
		// Only VLAN8021QDevice have parents that still allow us to
		// bridge them.
		// When anything else has a parent set, it shouldn't be used.
		return false, nil
	}
	parentDevice, err := dev.ParentDevice()
	if err != nil {
		// If we got an error here, we have some sort of
		// database inconsistency error.
		return false, err
	}
	if parentDevice.Type() == corenetwork.EthernetDevice || parentDevice.Type() == corenetwork.BondDevice {
		// A plain VLAN device with a direct parent
		// of its underlying ethernet device.
		return true, nil
	}
	return false, nil
}

// The general policy is to:
// 1.  Add br- to device name (to keep current behaviour),
//     if it does not fit in 15 characters then:
// 2.  Add b- to device name, if it doesn't fit in 15 characters then:
// 3a. For devices starting in 'en' remove 'en' and add 'b-'
// 3b. For all other devices
//     'b-' + 6-char hash of name + '-' + last 6 chars of name
// 4.  If using the device name directly always replace '.' with '-'
//     to make sure that bridges from VLANs won't break
func BridgeNameForDevice(device string) string {
	device = strings.Replace(device, ".", "-", -1)
	switch {
	case len(device) < 13:
		return fmt.Sprintf("br-%s", device)
	case len(device) == 13:
		return fmt.Sprintf("b-%s", device)
	case device[:2] == "en":
		return fmt.Sprintf("b-%s", device[2:])
	default:
		hash := crc32.Checksum([]byte(device), crc32.IEEETable) & 0xffffff
		return fmt.Sprintf("b-%0.6x-%s", hash, device[len(device)-6:])
	}
}

// PopulateContainerLinkLayerDevices sets the link-layer devices of the input
// guest, setting each device to be a child of the corresponding bridge on the
// host machine.
// It also records when one of the desired spaces is available on the host
// machine, but not currently bridged.
func (p *BridgePolicy) PopulateContainerLinkLayerDevices(host Machine, guest Container) error {
	// TODO(jam): 20017-01-31 This doesn't quite feel right that we would be
	// defining devices that 'will' exist in the container, but don't exist
	// yet. If anything, this feels more like "Provider" level devices, because
	// it is defining the devices from the outside, not the inside.
	guestSpaces, devicesPerSpace, err := p.findSpacesAndDevicesForContainer(host, guest)
	if err != nil {
		return errors.Trace(err)
	}
	logger.Debugf("for container %q, found host devices spaces: %s", guest.Id(), formatDeviceMap(devicesPerSpace))
	localBridgeForType := map[instance.ContainerType]string{
		instance.LXD: network.DefaultLXDBridge,
		instance.KVM: network.DefaultKVMBridge,
	}
	spacesFound := set.NewStrings()
	devicesByName := make(map[string]LinkLayerDevice)
	bridgeDeviceNames := make([]string, 0)

	for spaceID, hostDevices := range devicesPerSpace {
		for _, hostDevice := range hostDevices {
			isFan := strings.HasPrefix(hostDevice.Name(), "fan-")
			wantThisDevice := isFan == (p.containerNetworkingMethod == "fan")
			deviceType, name := hostDevice.Type(), hostDevice.Name()
			if wantThisDevice && deviceType == corenetwork.BridgeDevice && !skippedDeviceNames.Contains(name) {
				devicesByName[name] = hostDevice
				bridgeDeviceNames = append(bridgeDeviceNames, name)
				spacesFound.Add(spaceID)
			}
		}
	}

	guestSpaceSet := set.NewStrings(guestSpaces.IDs()...)
	missingSpaces := guestSpaceSet.Difference(spacesFound)

	// Check if we are missing the default space and can fill it in with a local bridge
	if len(missingSpaces) == 1 &&
		missingSpaces.Contains(corenetwork.DefaultSpaceId) &&
		p.containerNetworkingMethod == "local" {
		localBridgeName := localBridgeForType[guest.ContainerType()]
		for _, hostDevice := range devicesPerSpace[corenetwork.DefaultSpaceId] {
			name := hostDevice.Name()
			if hostDevice.Type() == corenetwork.BridgeDevice && name == localBridgeName {
				missingSpaces.Remove(corenetwork.DefaultSpaceId)
				devicesByName[name] = hostDevice
				bridgeDeviceNames = append(bridgeDeviceNames, name)
				spacesFound.Add(corenetwork.DefaultSpaceId)
			}
		}
	}

	if len(missingSpaces) > 0 && len(bridgeDeviceNames) == 0 {
		logger.Warningf("container %q wants spaces %s could not find host %q bridges for %s, found bridges %s",
			guest.Id(), network.QuoteSpaceSet(guestSpaceSet),
			host.Id(), network.QuoteSpaceSet(missingSpaces), bridgeDeviceNames)
		return errors.Errorf("unable to find host bridge for space(s) %s for container %q",
			network.QuoteSpaceSet(missingSpaces), guest.Id())
	}

	sortedBridgeDeviceNames := network.NaturallySortDeviceNames(bridgeDeviceNames...)
	logger.Debugf("for container %q using host machine %q bridge devices: %s",
		guest.Id(), host.Id(), network.QuoteSpaces(sortedBridgeDeviceNames))
	containerDevicesArgs := make([]state.LinkLayerDeviceArgs, len(bridgeDeviceNames))

	for i, hostBridgeName := range sortedBridgeDeviceNames {
		hostBridge := devicesByName[hostBridgeName]
		newLLD, err := hostBridge.EthernetDeviceForBridge(fmt.Sprintf("eth%d", i))
		if err != nil {
			return errors.Trace(err)
		}
		containerDevicesArgs[i] = newLLD
	}
	logger.Debugf("prepared container %q network config: %+v", guest.Id(), containerDevicesArgs)

	if err := guest.SetLinkLayerDevices(containerDevicesArgs...); err != nil {
		return errors.Trace(err)
	}

	logger.Debugf("container %q network config set", guest.Id())
	return nil
}

func formatDeviceMap(spacesToDevices map[string][]LinkLayerDevice) string {
	spaceIDs := make([]string, len(spacesToDevices))
	i := 0
	for spaceID := range spacesToDevices {
		spaceIDs[i] = spaceID
		i++
	}
	sort.Strings(spaceIDs)
	var out []string
	for _, id := range spaceIDs {
		start := fmt.Sprintf("%q:[", id)
		devices := spacesToDevices[id]
		deviceNames := make([]string, len(devices))
		for i, dev := range devices {
			deviceNames[i] = dev.Name()
		}
		deviceNames = network.NaturallySortDeviceNames(deviceNames...)
		quotedNames := make([]string, len(deviceNames))
		for i, name := range deviceNames {
			quotedNames[i] = fmt.Sprintf("%q", name)
		}
		out = append(out, start+strings.Join(quotedNames, ",")+"]")
	}
	return "map{" + strings.Join(out, ", ") + "}"
}
