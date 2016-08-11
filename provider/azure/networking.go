// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package azure

import (
	"fmt"
	"net"
	"path"

	"github.com/Azure/azure-sdk-for-go/arm/compute"
	"github.com/Azure/azure-sdk-for-go/arm/network"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/juju/errors"
	"github.com/juju/utils/set"

	"github.com/juju/juju/provider/azure/internal/iputils"
)

const (
	// internalNetworkName is the name of the virtual network that all
	// Juju machines within a resource group are connected to.
	//
	// Each resource group is given its own network, subnet and network
	// security group to manage. Each resource group will have its own
	// private 10.0.0.0/16 network.
	internalNetworkName = "juju-internal-network"

	// internalSubnetName is the name of the subnet that each machine's
	// primary NIC is attached to.
	internalSubnetName = "juju-internal-subnet"

	// internalSecurityGroupName is the name of the network security
	// group that each machine's primary (internal network) NIC is
	// attached to.
	internalSecurityGroupName = "juju-internal-nsg"
)

const (
	// securityRuleInternalMin is the beginning of the range of
	// internal security group rules defined by Juju.
	securityRuleInternalMin = 100

	// securityRuleInternalMax is the end of the range of internal
	// security group rules defined by Juju.
	securityRuleInternalMax = 199

	// securityRuleMax is the maximum allowable security rule
	// priority.
	securityRuleMax = 4096
)

const (
	// securityRuleInternalSSHInbound is the priority of the
	// security rule that allows inbound SSH access to all
	// machines.
	securityRuleInternalSSHInbound = securityRuleInternalMin + iota
)

var sshSecurityRule = network.SecurityRule{
	Name: to.StringPtr("SSHInbound"),
	Properties: &network.SecurityRulePropertiesFormat{
		Description:              to.StringPtr("Allow SSH access to all machines"),
		Protocol:                 network.TCP,
		SourceAddressPrefix:      to.StringPtr("*"),
		SourcePortRange:          to.StringPtr("*"),
		DestinationAddressPrefix: to.StringPtr("*"),
		DestinationPortRange:     to.StringPtr("22"),
		Access:                   network.Allow,
		Priority:                 to.Int32Ptr(securityRuleInternalSSHInbound),
		Direction:                network.Inbound,
	},
}

func createInternalVirtualNetwork(
	callAPI callAPIFunc,
	client network.ManagementClient,
	resourceGroup string,
	location string,
	tags map[string]string,
) (*network.VirtualNetwork, error) {
	addressPrefixes := []string{"10.0.0.0/16"}
	virtualNetworkParams := network.VirtualNetwork{
		Location: to.StringPtr(location),
		Tags:     to.StringMapPtr(tags),
		Properties: &network.VirtualNetworkPropertiesFormat{
			AddressSpace: &network.AddressSpace{&addressPrefixes},
		},
	}
	logger.Debugf("creating virtual network %q", internalNetworkName)
	vnetClient := network.VirtualNetworksClient{client}
	if err := callAPI(func() (autorest.Response, error) {
		return vnetClient.CreateOrUpdate(
			resourceGroup, internalNetworkName, virtualNetworkParams,
			nil, // abort channel
		)
	}); err != nil {
		return nil, errors.Annotatef(err, "creating virtual network %q", internalNetworkName)
	}

	var vnet network.VirtualNetwork
	if err := callAPI(func() (autorest.Response, error) {
		var err error
		vnet, err = vnetClient.Get(resourceGroup, internalNetworkName, "")
		return vnet.Response, err
	}); err != nil {
		return nil, errors.Annotatef(err, "creating virtual network %q", internalNetworkName)
	}
	return &vnet, nil
}

// createInternalSubnet creates an internal subnet for the specified resource group,
// within the specified virtual network.
//
// NOTE(axw) this method expects an up-to-date VirtualNetwork, and expects that are
// no concurrent subnet additions to the virtual network. At the moment we have only
// three places where we modify subnets: at bootstrap, when a new environment is
// created, and when an environment is destroyed.
func createInternalSubnet(
	callAPI callAPIFunc,
	client network.ManagementClient,
	resourceGroup string,
	vnet *network.VirtualNetwork,
	location string,
	tags map[string]string,
) (*network.Subnet, error) {

	nextAddressPrefix := (*vnet.Properties.AddressSpace.AddressPrefixes)[0]
	if vnet.Properties.Subnets != nil {
		if len(*vnet.Properties.Subnets) == len(*vnet.Properties.AddressSpace.AddressPrefixes) {
			return nil, errors.Errorf(
				"no available address prefixes in vnet %q",
				to.String(vnet.Name),
			)
		}
		addressPrefixesInUse := make(set.Strings)
		for _, subnet := range *vnet.Properties.Subnets {
			addressPrefixesInUse.Add(to.String(subnet.Properties.AddressPrefix))
		}
		for _, addressPrefix := range *vnet.Properties.AddressSpace.AddressPrefixes {
			if !addressPrefixesInUse.Contains(addressPrefix) {
				nextAddressPrefix = addressPrefix
				break
			}
		}
	}

	// Create a network security group for the environment. There is only
	// one NSG per environment (there's a limit of 100 per subscription),
	// in which we manage rules for each exposed machine.
	securityRules := []network.SecurityRule{sshSecurityRule}
	securityGroupParams := network.SecurityGroup{
		Location: to.StringPtr(location),
		Tags:     to.StringMapPtr(tags),
		Properties: &network.SecurityGroupPropertiesFormat{
			SecurityRules: &securityRules,
		},
	}
	securityGroupClient := network.SecurityGroupsClient{client}
	securityGroupName := internalSecurityGroupName
	logger.Debugf("creating security group %q", securityGroupName)
	if err := callAPI(func() (autorest.Response, error) {
		return securityGroupClient.CreateOrUpdate(
			resourceGroup, securityGroupName, securityGroupParams,
			nil, // abort channel
		)
	}); err != nil {
		return nil, errors.Annotatef(err, "creating security group %q", securityGroupName)
	}

	var nsg network.SecurityGroup
	if err := callAPI(func() (autorest.Response, error) {
		var err error
		nsg, err = securityGroupClient.Get(resourceGroup, securityGroupName, "")
		return nsg.Response, err
	}); err != nil {
		return nil, errors.Annotatef(err, "creating security group %q", securityGroupName)
	}

	// Now create a subnet with the next available address prefix, and
	// associate the subnet with the NSG created above.
	subnetName := internalSubnetName
	subnetParams := network.Subnet{
		Properties: &network.SubnetPropertiesFormat{
			AddressPrefix:        to.StringPtr(nextAddressPrefix),
			NetworkSecurityGroup: &nsg,
		},
	}
	logger.Debugf("creating subnet %q (%s)", subnetName, nextAddressPrefix)
	subnetClient := network.SubnetsClient{client}
	if err := callAPI(func() (autorest.Response, error) {
		return subnetClient.CreateOrUpdate(
			resourceGroup, internalNetworkName, subnetName, subnetParams,
			nil, // abort channel
		)
	}); err != nil {
		return nil, errors.Annotatef(err, "creating subnet %q", subnetName)
	}
	return getInternalSubnet(callAPI, client, resourceGroup)
}

func getInternalSubnet(
	callAPI callAPIFunc,
	client network.ManagementClient,
	resourceGroup string,
) (*network.Subnet, error) {
	subnetClient := network.SubnetsClient{client}
	vnetName := internalNetworkName
	subnetName := internalSubnetName
	var subnet network.Subnet
	if err := callAPI(func() (autorest.Response, error) {
		var err error
		subnet, err = subnetClient.Get(resourceGroup, vnetName, subnetName, "")
		return subnet.Response, err
	}); err != nil {
		return nil, errors.Annotate(err, "getting internal subnet")
	}
	return &subnet, nil
}

func newNetworkProfile(
	callAPI callAPIFunc,
	client network.ManagementClient,
	vmName string,
	apiPort *int,
	internalSubnet *network.Subnet,
	resourceGroup string,
	location string,
	tags map[string]string,
) (*compute.NetworkProfile, error) {
	logger.Debugf("creating network profile for %q", vmName)

	// Create a public IP for the NIC. Public IP addresses are dynamic.
	logger.Debugf("- allocating public IP address")
	pipClient := network.PublicIPAddressesClient{client}
	publicIPAddressParams := network.PublicIPAddress{
		Location: to.StringPtr(location),
		Tags:     to.StringMapPtr(tags),
		Properties: &network.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: network.Dynamic,
		},
	}
	publicIPAddressName := vmName + "-public-ip"
	if err := callAPI(func() (autorest.Response, error) {
		return pipClient.CreateOrUpdate(
			resourceGroup, publicIPAddressName, publicIPAddressParams,
			nil, // abort channel
		)
	}); err != nil {
		return nil, errors.Annotatef(err, "creating public IP address for %q", vmName)
	}

	var publicIPAddress network.PublicIPAddress
	if err := callAPI(func() (autorest.Response, error) {
		var err error
		publicIPAddress, err = pipClient.Get(resourceGroup, publicIPAddressName, "")
		return publicIPAddress.Response, err
	}); err != nil {
		return nil, errors.Annotatef(err, "getting public IP address for %q", vmName)
	}

	// Determine the next available private IP address.
	nicClient := network.InterfacesClient{client}
	privateIPAddress, err := nextSubnetIPAddress(nicClient, resourceGroup, internalSubnet)
	if err != nil {
		return nil, errors.Annotatef(err, "querying private IP addresses")
	}

	// Create a primary NIC for the machine. This needs to be static, so
	// that we can create security rules that don't become invalid.
	logger.Debugf("- creating primary NIC")
	ipConfigurations := []network.InterfaceIPConfiguration{{
		Name: to.StringPtr("primary"),
		Properties: &network.InterfaceIPConfigurationPropertiesFormat{
			PrivateIPAddress:          to.StringPtr(privateIPAddress),
			PrivateIPAllocationMethod: network.Static,
			Subnet:          internalSubnet,
			PublicIPAddress: &publicIPAddress,
		},
	}}
	primaryNicName := vmName + "-primary"
	primaryNicParams := network.Interface{
		Location: to.StringPtr(location),
		Tags:     to.StringMapPtr(tags),
		Properties: &network.InterfacePropertiesFormat{
			IPConfigurations: &ipConfigurations,
		},
	}
	if err := callAPI(func() (autorest.Response, error) {
		return nicClient.CreateOrUpdate(
			resourceGroup, primaryNicName, primaryNicParams,
			nil, // abort channel
		)
	}); err != nil {
		return nil, errors.Annotatef(err, "creating network interface for %q", vmName)
	}

	var primaryNic network.Interface
	if err := callAPI(func() (autorest.Response, error) {
		var err error
		primaryNic, err = nicClient.Get(resourceGroup, primaryNicName, "")
		return primaryNic.Response, err
	}); err != nil {
		return nil, errors.Annotatef(err, "getting network interface for %q", vmName)
	}

	// Create a network security rule for the machine if we need to open
	// the API server port.
	if apiPort != nil {
		logger.Debugf("- querying network security group")
		securityGroupClient := network.SecurityGroupsClient{client}
		securityGroupName := internalSecurityGroupName
		var securityGroup network.SecurityGroup
		if err := callAPI(func() (autorest.Response, error) {
			var err error
			securityGroup, err = securityGroupClient.Get(resourceGroup, securityGroupName, "")
			return securityGroup.Response, err
		}); err != nil {
			return nil, errors.Annotate(err, "querying network security group")
		}

		// NOTE(axw) this looks like TOCTTOU race territory, but it's
		// safe because we only allocate/deallocate rules in this
		// range during machine (de)provisioning, which is managed by
		// a single goroutine. Non-internal ports are managed by the
		// firewaller exclusively.
		nextPriority, err := nextSecurityRulePriority(
			securityGroup,
			securityRuleInternalSSHInbound+1,
			securityRuleInternalMax,
		)
		if err != nil {
			return nil, errors.Trace(err)
		}

		apiSecurityRuleName := fmt.Sprintf("%s-api", vmName)
		apiSecurityRule := network.SecurityRule{
			Name: to.StringPtr(apiSecurityRuleName),
			Properties: &network.SecurityRulePropertiesFormat{
				Description:              to.StringPtr("Allow API access to server machines"),
				Protocol:                 network.TCP,
				SourceAddressPrefix:      to.StringPtr("*"),
				SourcePortRange:          to.StringPtr("*"),
				DestinationAddressPrefix: to.StringPtr(privateIPAddress),
				DestinationPortRange:     to.StringPtr(fmt.Sprint(*apiPort)),
				Access:                   network.Allow,
				Priority:                 to.Int32Ptr(nextPriority),
				Direction:                network.Inbound,
			},
		}
		logger.Debugf("- creating API network security rule")
		securityRuleClient := network.SecurityRulesClient{client}
		if err := callAPI(func() (autorest.Response, error) {
			return securityRuleClient.CreateOrUpdate(
				resourceGroup, securityGroupName, apiSecurityRuleName, apiSecurityRule,
				nil, // abort channel
			)
		}); err != nil {
			return nil, errors.Annotate(err, "creating API network security rule")
		}
	}

	// For now we only attach a single, flat network to each machine.
	networkInterfaces := []compute.NetworkInterfaceReference{{
		ID: primaryNic.ID,
		Properties: &compute.NetworkInterfaceReferenceProperties{
			Primary: to.BoolPtr(true),
		},
	}}
	return &compute.NetworkProfile{&networkInterfaces}, nil
}

// nextSecurityRulePriority returns the next available priority in the given
// security group within a specified range.
func nextSecurityRulePriority(group network.SecurityGroup, min, max int32) (int32, error) {
	if group.Properties.SecurityRules == nil {
		return min, nil
	}
	for p := min; p <= max; p++ {
		var found bool
		for _, rule := range *group.Properties.SecurityRules {
			if to.Int32(rule.Properties.Priority) == p {
				found = true
				break
			}
		}
		if !found {
			return p, nil
		}
	}
	return -1, errors.Errorf(
		"no priorities available in the range [%d, %d]", min, max,
	)
}

// nextSubnetIPAddress returns the next available IP address in the given subnet.
func nextSubnetIPAddress(
	nicClient network.InterfacesClient,
	resourceGroup string,
	subnet *network.Subnet,
) (string, error) {
	_, ipnet, err := net.ParseCIDR(to.String(subnet.Properties.AddressPrefix))
	if err != nil {
		return "", errors.Annotate(err, "parsing subnet prefix")
	}
	results, err := nicClient.List(resourceGroup)
	if err != nil {
		return "", errors.Annotate(err, "listing NICs")
	}
	// Azure reserves the first 4 addresses in the subnet.
	var ipsInUse []net.IP
	if results.Value != nil {
		ipsInUse = make([]net.IP, 0, len(*results.Value))
		for _, item := range *results.Value {
			if item.Properties.IPConfigurations == nil {
				continue
			}
			for _, ipConfiguration := range *item.Properties.IPConfigurations {
				if to.String(ipConfiguration.Properties.Subnet.ID) != to.String(subnet.ID) {
					continue
				}
				ip := net.ParseIP(to.String(ipConfiguration.Properties.PrivateIPAddress))
				if ip != nil {
					ipsInUse = append(ipsInUse, ip)
				}
			}
		}
	}
	ip, err := iputils.NextSubnetIP(ipnet, ipsInUse)
	if err != nil {
		return "", errors.Trace(err)
	}
	return ip.String(), nil
}

// internalSubnetId returns the Azure resource ID of the internal network
// subnet for the specified resource group.
func internalSubnetId(resourceGroup, subscriptionId string) string {
	return path.Join(
		"/subscriptions", subscriptionId,
		"resourceGroups", resourceGroup,
		"providers/Microsoft.Network/virtualNetworks",
		internalNetworkName, "subnets", internalSubnetName,
	)
}
