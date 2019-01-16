/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ec2

import (
	"encoding/base64"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/apis/awsprovider/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/actuators"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/converters"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/filter"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/services/awserrors"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/services/certificates"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/services/userdata"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/tags"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/record"
)

// InstanceByTags returns the existing instance or nothing if it doesn't exist.
func (s *Service) InstanceByTags(machine *actuators.MachineScope) (*v1alpha1.Instance, error) {
	klog.V(2).Infof("Looking for existing instance for machine %q in cluster %q", machine.Name(), s.scope.Name())

	input := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			filter.EC2.ClusterOwned(s.scope.Name()),
			filter.EC2.Name(machine.Name()),
			filter.EC2.InstanceStates(ec2.InstanceStateNamePending, ec2.InstanceStateNameRunning),
		},
	}

	out, err := s.scope.EC2.DescribeInstances(input)
	switch {
	case awserrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, errors.Wrap(err, "failed to describe instances by tags")
	}

	// TODO: currently just returns the first matched instance, need to
	// better rationalize how to find the right instance to return if multiple
	// match
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			return converters.SDKToInstance(inst), nil
		}
	}

	return nil, nil
}

// InstanceIfExists returns the existing instance or nothing if it doesn't exist.
func (s *Service) InstanceIfExists(id string) (*v1alpha1.Instance, error) {
	klog.V(2).Infof("Looking for instance %q", id)

	input := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(id)},
		Filters:     []*ec2.Filter{filter.EC2.InstanceStates(ec2.InstanceStateNamePending, ec2.InstanceStateNameRunning)},
	}

	out, err := s.scope.EC2.DescribeInstances(input)
	switch {
	case awserrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, errors.Wrapf(err, "failed to describe instance: %q", id)
	}

	if len(out.Reservations) > 0 && len(out.Reservations[0].Instances) > 0 {
		return converters.SDKToInstance(out.Reservations[0].Instances[0]), nil
	}

	return nil, nil
}

// createInstance runs an ec2 instance.
func (s *Service) createInstance(machine *actuators.MachineScope, bootstrapToken string) (*v1alpha1.Instance, error) {
	klog.V(2).Infof("Creating a new instance for machine %q", machine.Name())
	var iAMProfile string
	if machine.MachineConfig.IAMInstanceProfile.ID != nil {
		iAMProfile = *machine.MachineConfig.IAMInstanceProfile.ID
	} else {
		iAMProfile = *machine.MachineConfig.IAMInstanceProfile.ARN
	}
	input := &v1alpha1.Instance{
		Type:       machine.MachineConfig.InstanceType,
		IAMProfile: iAMProfile,
	}

	input.Tags = tags.Build(tags.BuildParams{
		ClusterName: s.scope.Scope.Name(),
		Lifecycle:   tags.ResourceLifecycleOwned,
		Name:        aws.String(machine.Name()),
		Role:        aws.String(machine.Role()),
	})
	//TODO (vikasc): do in proper way using BuildParams as above
	//TODO (vikasc): use clusterName only and not cluster-id. Remove cluster-id once changes are done in MAO/installer to use clusterName and not ID
	input.Tags["clusterid"] = machine.ClusterID()
	input.Tags["kubernetes.io/cluster/"+machine.ClusterID()] = "owned"

	var err error
	// Pick image from the machine configuration, or use a default one.
	if machine.MachineConfig.AMI.ID != nil {
		input.ImageID = *machine.MachineConfig.AMI.ID
	} else {
		input.ImageID, err = s.defaultAMILookup("ubuntu", "18.04", machine.Machine.Spec.Versions.Kubelet)
		if err != nil {
			return nil, err
		}
	}

	var networkInterface v1alpha1.NetworkInterface
	// Pick subnet from the machine configuration, or default to the first private available.
	if machine.MachineConfig.Subnet != nil {
		subnetID := ""
		if machine.MachineConfig.Subnet.ID != nil {
			subnetID = *machine.MachineConfig.Subnet.ID
		} else {
			var filters []v1alpha1.Filter
			availabilityZone := machine.MachineConfig.Placement.AvailabilityZone
			if availabilityZone != "" {
				// Improve error logging for better user experience.
				// Otherwise, during the process of minimizing API calls, this is a good
				// candidate for removal.
				_, err := machine.EC2.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{
					ZoneNames: []*string{aws.String(availabilityZone)},
				})
				if err != nil {
					klog.Errorf("error describing availability zones: %v", err)
					return nil, errors.Errorf("error describing availability zones: %v", err)
				}
				filters = append(filters, v1alpha1.Filter{Name: "availabilityZone", Values: []string{availabilityZone}})
			}
			filters = append(filters, machine.MachineConfig.Subnet.Filters...)
			klog.Info("Describing subnets based on filters: %v", filters)
			describeSubnetRequest := ec2.DescribeSubnetsInput{
				Filters: buildEC2Filters(filters),
			}
			describeSubnetResult, err := machine.AWSClients.EC2.DescribeSubnets(&describeSubnetRequest)
			if err != nil {
				klog.Errorf("error describing subnetes: %v", err)
				return nil, errors.Errorf("error describing subnets: %v", err)
			}
			klog.Infof("Describing subnetes: %v", describeSubnetResult.Subnets)
			for _, n := range describeSubnetResult.Subnets {
				subnetID = *n.SubnetId
				break
			}
		}
		// build list of networkInterfaces (just 1 for now)
		networkInterface.DeviceIndex = aws.Int64(machine.MachineConfig.DeviceIndex)
		networkInterface.AssociatePublicIpAddress = machine.MachineConfig.PublicIP
		networkInterface.SubnetId = aws.String(subnetID)
	} else {
		sns := s.scope.Subnets().FilterPrivate()
		if len(sns) == 0 {
			return nil, awserrors.NewFailedDependency(
				errors.Errorf("failed to run machine %q, no subnets available", machine.Name()),
			)
		}
		input.SubnetID = sns[0].ID
	}

	if s.scope.ClusterConfig != nil && len(s.scope.ClusterConfig.CACertificate) == 0 {
		return nil, awserrors.NewFailedDependency(
			errors.New("failed to run controlplane, missing CACertificate"),
		)
	}

	if s.scope.Network() != nil && s.scope.Network().APIServerELB.DNSName == "" {
		return nil, awserrors.NewFailedDependency(
			errors.New("failed to run controlplane, APIServer ELB not available"),
		)
	}

	// apply values based on the role of the machine
	if machine.Role() == "controlplane" {

		if s.scope.Scope.SecurityGroups()[v1alpha1.SecurityGroupControlPlane] == nil {
			return nil, awserrors.NewFailedDependency(
				errors.New("failed to run controlplane, security group not available"),
			)
		}

		if len(s.scope.ClusterConfig.CAPrivateKey) == 0 {
			return nil, awserrors.NewFailedDependency(
				errors.New("failed to run controlplane, missing CAPrivateKey"),
			)
		}

		userData, err := userdata.NewControlPlane(&userdata.ControlPlaneInput{
			CACert:            string(s.scope.ClusterConfig.CACertificate),
			CAKey:             string(s.scope.ClusterConfig.CAPrivateKey),
			ELBAddress:        s.scope.Network().APIServerELB.DNSName,
			ClusterName:       s.scope.Name(),
			PodSubnet:         s.scope.Cluster.Spec.ClusterNetwork.Pods.CIDRBlocks[0],
			ServiceSubnet:     s.scope.Cluster.Spec.ClusterNetwork.Services.CIDRBlocks[0],
			ServiceDomain:     s.scope.Cluster.Spec.ClusterNetwork.ServiceDomain,
			KubernetesVersion: machine.Machine.Spec.Versions.ControlPlane,
		})

		if err != nil {
			return input, err
		}

		input.UserData = aws.String(userData)
		input.SecurityGroupIDs = append(input.SecurityGroupIDs, s.scope.Scope.SecurityGroups()[v1alpha1.SecurityGroupControlPlane].ID)
	}

	if machine.Role() == "node" {
		if s.scope.Scope.Cluster != nil {
			input.SecurityGroupIDs = append(input.SecurityGroupIDs, s.scope.Scope.SecurityGroups()[v1alpha1.SecurityGroupNode].ID)
		}
		for _, id := range s.scope.SecurityGroups() {
			input.SecurityGroupIDs = append(input.SecurityGroupIDs, id)
		}
		klog.Infof("SecurityGroups: %v", input.SecurityGroupIDs)

		for _, group := range input.SecurityGroupIDs {
			networkInterface.Groups = append(networkInterface.Groups, &group)
		}
		input.NetworkInterfaces = []*v1alpha1.NetworkInterface{&networkInterface}

		userDataSecretKey := "userData"

		if s.scope.Scope.Cluster == nil && machine.MachineConfig.UserDataSecret != nil {
			kubeClient := *machine.Scope.KubeClient
			userDataSecret, err := kubeClient.CoreV1().Secrets(machine.Namespace()).Get(machine.MachineConfig.UserDataSecret.Name, metav1.GetOptions{})
			if err != nil {
				return input, err
			}
			if data, exists := userDataSecret.Data[userDataSecretKey]; exists {
				input.UserData = aws.String(base64.StdEncoding.EncodeToString(data))
			} else {
				klog.Warningf("Secret %v/%v does not have %q field set. Thus, no user data applied when creating an instance.", machine.Namespace, machine.MachineConfig.UserDataSecret.Name, userDataSecretKey)
			}
		} else {
			caCertHash, err := certificates.GenerateCertificateHash(s.scope.ClusterConfig.CACertificate)
			if err != nil {
				return input, err
			}
			userData, err := userdata.NewNode(&userdata.NodeInput{
				CACertHash:     caCertHash,
				BootstrapToken: bootstrapToken,
				ELBAddress:     s.scope.Network().APIServerELB.DNSName,
			})

			if err != nil {
				return input, err
			}
			input.UserData = &userData
		}
	}

	// Pick SSH key, if any.
	if machine.MachineConfig.KeyName != "" {
		input.KeyName = aws.String(machine.MachineConfig.KeyName)
	} else {
		//TODO(vikasc): use default key
		//input.KeyName = aws.String(defaultSSHKeyName)
	}

	out, err := s.runInstance(machine.Role(), input)
	if err != nil {
		return nil, err
	}

	record.Eventf(machine.Machine, "CreatedInstance", "Created new %s instance with id %q", machine.Role(), out.ID)
	return out, nil
}

func buildEC2Filters(inputFilters []v1alpha1.Filter) []*ec2.Filter {
	filters := make([]*ec2.Filter, len(inputFilters))
	for i, f := range inputFilters {
		values := make([]*string, len(f.Values))
		for j, v := range f.Values {
			values[j] = aws.String(v)
		}
		filters[i] = &ec2.Filter{
			Name:   aws.String(f.Name),
			Values: values,
		}
	}
	return filters
}

// TerminateInstance terminates an EC2 instance.
// Returns nil on success, error in all other cases.
func (s *Service) TerminateInstance(instanceID string) error {
	klog.V(2).Infof("Attempting to terminate instance with id %q", instanceID)

	input := &ec2.TerminateInstancesInput{
		InstanceIds: aws.StringSlice([]string{instanceID}),
	}

	if _, err := s.scope.EC2.TerminateInstances(input); err != nil {
		return errors.Wrapf(err, "failed to terminate instance with id %q", instanceID)
	}

	klog.V(2).Infof("Terminated instance with id %q", instanceID)
	if s.scope.Cluster != nil {
		record.Eventf(s.scope.Cluster, "DeletedInstance", "Terminated instance %q", instanceID)
	} else {
		record.Eventf(s.scope.Machine, "DeletedInstance", "Terminated instance %q", instanceID)
	}
	return nil
}

// TerminateInstanceAndWait terminates and waits
// for an EC2 instance to terminate.
func (s *Service) TerminateInstanceAndWait(instanceID string) error {
	if err := s.TerminateInstance(instanceID); err != nil {
		return err
	}

	klog.V(2).Infof("Waiting for EC2 instance with id %q to terminate", instanceID)

	input := &ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice([]string{instanceID}),
	}

	if err := s.scope.EC2.WaitUntilInstanceTerminated(input); err != nil {
		return errors.Wrapf(err, "failed to wait for instance %q termination", instanceID)
	}

	return nil
}

// CreateOrGetMachine will either return an existing instance or create and return an instance.
func (s *Service) CreateOrGetMachine(machine *actuators.MachineScope, bootstrapToken string) (*v1alpha1.Instance, error) {
	klog.V(2).Infof("Attempting to create or get machine %q", machine.Name())

	// instance id exists, try to get it
	if machine.MachineStatus.InstanceID != nil {
		klog.V(2).Infof("Looking up machine %q by id %q", machine.Name(), *machine.MachineStatus.InstanceID)

		instance, err := s.InstanceIfExists(*machine.MachineStatus.InstanceID)
		if err != nil && !awserrors.IsNotFound(err) {
			return nil, errors.Wrapf(err, "failed to look up machine %q by id %q", machine.Name(), *machine.MachineStatus.InstanceID)
		} else if err == nil && instance != nil {
			return instance, nil
		}
	}

	klog.V(2).Infof("Looking up machine %q by tags", machine.Name())
	instance, err := s.InstanceByTags(machine)
	if err != nil && !awserrors.IsNotFound(err) {
		return nil, errors.Wrapf(err, "failed to query machine %q instance by tags", machine.Name())
	} else if err == nil && instance != nil {
		return instance, nil
	}

	return s.createInstance(machine, bootstrapToken)
}

func (s *Service) runInstance(role string, i *v1alpha1.Instance) (*v1alpha1.Instance, error) {
	input := &ec2.RunInstancesInput{
		InstanceType: aws.String(i.Type),
		ImageId:      aws.String(i.ImageID),
		KeyName:      i.KeyName,
		EbsOptimized: i.EBSOptimized,
		MaxCount:     aws.Int64(1),
		MinCount:     aws.Int64(1),
		UserData:     i.UserData,
	}
	if len(i.NetworkInterfaces) == 0 {
		input.SubnetId = aws.String(i.SubnetID)
		if len(i.SecurityGroupIDs) > 0 {
			input.SecurityGroupIds = aws.StringSlice(i.SecurityGroupIDs)
		}
	}
	for _, intf := range i.NetworkInterfaces {
		var newIntf ec2.InstanceNetworkInterfaceSpecification
		newIntf.Groups = intf.Groups
		newIntf.DeviceIndex = intf.DeviceIndex
		newIntf.AssociatePublicIpAddress = intf.AssociatePublicIpAddress
		newIntf.SubnetId = intf.SubnetId
		input.NetworkInterfaces = append(input.NetworkInterfaces, &newIntf)

	}

	if i.IAMProfile != "" {
		input.IamInstanceProfile = &ec2.IamInstanceProfileSpecification{
			Name: aws.String(i.IAMProfile),
		}
	}

	if len(i.Tags) > 0 {
		spec := &ec2.TagSpecification{ResourceType: aws.String(ec2.ResourceTypeInstance)}
		for key, value := range i.Tags {
			spec.Tags = append(spec.Tags, &ec2.Tag{
				Key:   aws.String(key),
				Value: aws.String(value),
			})
		}

		input.TagSpecifications = append(input.TagSpecifications, spec)
	}
	input.TagSpecifications = append(input.TagSpecifications, &ec2.TagSpecification{
		ResourceType: aws.String("volume"),
		Tags:         []*ec2.Tag{{Key: aws.String("clusterid"), Value: aws.String(s.scope.ClusterID())}},
	})

	out, err := s.scope.EC2.RunInstances(input)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to run instance: %v", i)
	}

	if len(out.Instances) == 0 {
		return nil, errors.Errorf("no instance returned for reservation %v", out.GoString())
	}

	s.scope.EC2.WaitUntilInstanceRunning(&ec2.DescribeInstancesInput{InstanceIds: []*string{out.Instances[0].InstanceId}})
	return converters.SDKToInstance(out.Instances[0]), nil
}

// UpdateInstanceSecurityGroups modifies the security groups of the given
// EC2 instance.
func (s *Service) UpdateInstanceSecurityGroups(instanceID string, ids []string) error {
	klog.V(2).Infof("Attempting to update security groups on instance %q", instanceID)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Groups:     aws.StringSlice(ids),
	}

	if _, err := s.scope.EC2.ModifyInstanceAttribute(input); err != nil {
		return errors.Wrapf(err, "failed to modify instance %q security groups", instanceID)
	}

	return nil
}

// UpdateResourceTags updates the tags for an instance.
// This will be called if there is anything to create (update) or delete.
// We may not always have to perform each action, so we check what we're
// receiving to avoid calling AWS if we don't need to.
func (s *Service) UpdateResourceTags(resourceID *string, create map[string]string, remove map[string]string) error {
	klog.V(2).Infof("Attempting to update tags on resource %q", *resourceID)

	// If we have anything to create or update
	if len(create) > 0 {
		klog.V(2).Infof("Attempting to create tags on resource %q", *resourceID)

		// Convert our create map into an array of *ec2.Tag
		createTagsInput := converters.MapToTags(create)

		// Create the CreateTags input.
		input := &ec2.CreateTagsInput{
			Resources: []*string{resourceID},
			Tags:      createTagsInput,
		}

		// Create/Update tags in AWS.
		if _, err := s.scope.EC2.CreateTags(input); err != nil {
			return errors.Wrapf(err, "failed to create tags for resource %q: %+v", *resourceID, create)
		}
	}

	// If we have anything to remove
	if len(remove) > 0 {
		klog.V(2).Infof("Attempting to delete tags on resource %q", *resourceID)

		// Convert our remove map into an array of *ec2.Tag
		removeTagsInput := converters.MapToTags(remove)

		// Create the DeleteTags input
		input := &ec2.DeleteTagsInput{
			Resources: []*string{resourceID},
			Tags:      removeTagsInput,
		}

		// Delete tags in AWS.
		if _, err := s.scope.EC2.DeleteTags(input); err != nil {
			return errors.Wrapf(err, "failed to delete tags for resource %q: %v", *resourceID, remove)
		}
	}

	return nil
}
