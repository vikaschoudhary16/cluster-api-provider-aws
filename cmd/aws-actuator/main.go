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

package main

// Tests individual AWS actuator actions. This is meant to be executed
// in a machine that has access to AWS either as an instance with the right role
// or creds in ~/.aws/credentials

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/user"
	"time"

	flag "github.com/spf13/pflag"

	goflag "flag"

	"github.com/golang/glog"
	"github.com/spf13/cobra"

	awsclient "sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/client"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"

	"github.com/ghodss/yaml"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"

	"k8s.io/client-go/kubernetes/scheme"

	"text/template"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/cluster-api-actuator-pkg/pkg/e2e/framework"
	"github.com/openshift/cluster-api-actuator-pkg/pkg/manifests"
	"sigs.k8s.io/cluster-api-provider-aws/cmd/aws-actuator/utils"
	awsclientwrapper "sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/actuators/machine"
	testutils "sigs.k8s.io/cluster-api-provider-aws/test/utils"
)

const (
	instanceIDAnnotation     = "cluster-operator.openshift.io/aws-instance-id"
	ami                      = "ami-03f6257a"
	region                   = "us-east-1"
	size                     = "t1.micro"
	awsCredentialsSecretName = "aws-credentials-secret"

	pollInterval           = 5 * time.Second
	timeoutPoolAWSInterval = 10 * time.Minute
)

func usage() {
	fmt.Printf("Usage: %s\n\n", os.Args[0])
}

var rootCmd = &cobra.Command{
	Use:   "aws-actuator-test",
	Short: "Test for Cluster API AWS actuator",
}

func createCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "create",
		Short: "Create machine instance for specified cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFlags(cmd); err != nil {
				return err
			}
			cluster, machine, awsCredentials, userData, err := readClusterResources(
				&manifestParams{
					ClusterID: cmd.Flag("environment-id").Value.String(),
				},
				cmd.Flag("cluster").Value.String(),
				cmd.Flag("machine").Value.String(),
				cmd.Flag("aws-credentials").Value.String(),
				cmd.Flag("userdata").Value.String(),
			)
			if err != nil {
				return err
			}

			actuator := utils.CreateActuator(machine, awsCredentials, userData)
			result, err := actuator.CreateMachine(cluster, machine)
			if err != nil {
				return err
			}
			fmt.Printf("Machine creation was successful! InstanceID: %s\n", *result.InstanceId)
			return nil
		},
	}
}

func deleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete",
		Short: "Delete machine instance",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFlags(cmd); err != nil {
				return err
			}
			cluster, machine, awsCredentials, userData, err := readClusterResources(
				&manifestParams{
					ClusterID: cmd.Flag("environment-id").Value.String(),
				},
				cmd.Flag("cluster").Value.String(),
				cmd.Flag("machine").Value.String(),
				cmd.Flag("aws-credentials").Value.String(),
				cmd.Flag("userdata").Value.String(),
			)
			if err != nil {
				return err
			}

			actuator := utils.CreateActuator(machine, awsCredentials, userData)
			err = actuator.DeleteMachine(cluster, machine)
			if err != nil {
				return err
			}
			fmt.Printf("Machine delete operation was successful.\n")
			return nil
		},
	}
}

func existsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "exists",
		Short: "Determine if underlying machine instance exists",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFlags(cmd); err != nil {
				return err
			}
			cluster, machine, awsCredentials, userData, err := readClusterResources(
				&manifestParams{
					ClusterID: cmd.Flag("environment-id").Value.String(),
				},
				cmd.Flag("cluster").Value.String(),
				cmd.Flag("machine").Value.String(),
				cmd.Flag("aws-credentials").Value.String(),
				cmd.Flag("userdata").Value.String(),
			)
			if err != nil {
				return err
			}

			actuator := utils.CreateActuator(machine, awsCredentials, userData)
			exists, err := actuator.Exists(context.TODO(), cluster, machine)
			if err != nil {
				return err
			}
			if exists {
				fmt.Printf("Underlying machine's instance exists.\n")
			} else {
				fmt.Printf("Underlying machine's instance not found.\n")
			}
			return nil
		},
	}
}

func readMachineManifest(manifestParams *manifestParams, manifestLoc string) (*clusterv1.Machine, error) {
	machine := &clusterv1.Machine{}
	manifestBytes, err := ioutil.ReadFile(manifestLoc)
	if err != nil {
		return nil, fmt.Errorf("unable to read %v: %v", manifestLoc, err)
	}

	t, err := template.New("machineuserdata").Parse(string(manifestBytes))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, *manifestParams)
	if err != nil {
		return nil, err
	}

	if err = yaml.Unmarshal(buf.Bytes(), &machine); err != nil {
		return nil, fmt.Errorf("unable to unmarshal %v: %v", manifestLoc, err)
	}

	return machine, nil
}

func createSecretAndWait(f *framework.Framework, secret *apiv1.Secret) error {
	_, err := f.KubeClient.CoreV1().Secrets(secret.Namespace).Create(secret)
	if err != nil {
		return err
	}

	err = wait.Poll(framework.PollInterval, framework.PoolTimeout, func() (bool, error) {
		_, err := f.KubeClient.CoreV1().Secrets(secret.Namespace).Get(secret.Name, metav1.GetOptions{})
		return err == nil, nil
	})
	return err
}

func bootstrapCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap kubernetes cluster with kubeadm",
		RunE: func(cmd *cobra.Command, args []string) error {
			machinePrefix := cmd.Flag("environment-id").Value.String()

			mastermachinepk := cmd.Flag("master-machine-private-key").Value.String()
			if mastermachinepk == "" {
				return fmt.Errorf("--master-machine-private-key needs to be set")
			}

			if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
				return fmt.Errorf("AWS_ACCESS_KEY_ID env needs to be set")
			}
			if os.Getenv("AWS_SECRET_ACCESS_KEY") == "" {
				return fmt.Errorf("AWS_SECRET_ACCESS_KEY env needs to be set")
			}

			testNamespace := &apiv1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test",
				},
			}

			testCluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      machinePrefix,
					Namespace: testNamespace.Name,
				},
				Spec: clusterv1.ClusterSpec{
					ClusterNetwork: clusterv1.ClusterNetworkingConfig{
						Services: clusterv1.NetworkRanges{
							CIDRBlocks: []string{"10.0.0.1/24"},
						},
						Pods: clusterv1.NetworkRanges{
							CIDRBlocks: []string{"10.0.0.1/24"},
						},
						ServiceDomain: "example.com",
					},
				},
			}

			awsCredentialsSecret := testutils.GenerateAwsCredentialsSecretFromEnv(awsCredentialsSecretName, testNamespace.Name)

			// Create master machine and verify the master node is ready
			masterUserDataSecret, err := manifests.MasterMachineUserDataSecret(
				"masteruserdatasecret",
				testNamespace.Name,
				[]string{"\\$(curl -s http://169.254.169.254/latest/meta-data/public-hostname)", "\\$(curl -s http://169.254.169.254/latest/meta-data/public-ipv4)"},
			)
			if err != nil {
				return err
			}

			masterMachineProviderConfig, err := testutils.MasterMachineProviderConfig(awsCredentialsSecret.Name, masterUserDataSecret.Name, testCluster.Name)
			if err != nil {
				return err
			}

			masterMachine := manifests.MasterMachine(testCluster.Name, testCluster.Namespace, masterMachineProviderConfig)

			glog.Infof("Creating master machine")

			actuator := utils.CreateActuator(masterMachine, awsCredentialsSecret, masterUserDataSecret)
			result, err := actuator.CreateMachine(testCluster, masterMachine)
			if err != nil {
				glog.Error(err)
				return err
			}

			glog.Infof("Master machine created with ipv4: %v, InstanceId: %v", *result.PrivateIpAddress, *result.InstanceId)

			masterMachinePrivateIP := ""
			err = wait.Poll(pollInterval, timeoutPoolAWSInterval, func() (bool, error) {
				glog.Info("Waiting for master machine PublicDNS")
				result, err := actuator.Describe(testCluster, masterMachine)
				if err != nil {
					glog.Info(err)
					return false, nil
				}

				glog.Infof("PublicDnsName: %v\n", *result.PublicDnsName)
				if *result.PublicDnsName == "" {
					return false, nil
				}

				masterMachinePrivateIP = *result.PrivateIpAddress
				return true, nil
			})
			if err != nil {
				glog.Errorf("Unable to get DNS name: %v", err)
				return err
			}

			f := framework.Framework{
				SSH: &framework.SSHConfig{
					Key:  mastermachinepk,
					User: "ec2-user",
				},
			}

			objList := []runtime.Object{awsCredentialsSecret}
			fakeKubeClient := kubernetesfake.NewSimpleClientset(objList...)
			awsClient, err := awsclient.NewClient(fakeKubeClient, awsCredentialsSecret.Name, awsCredentialsSecret.Namespace, region)
			if err != nil {
				glog.Errorf("Unable to create aws client: %v", err)
				return err
			}

			acw := awsclientwrapper.NewAwsClientWrapper(awsClient)
			glog.Infof("Collecting master kubeconfig")
			restConfig, err := f.GetMasterMachineRestConfig(masterMachine, acw)
			if err != nil {
				glog.Errorf("Unable to pull kubeconfig: %v", err)
				return err
			}

			clusterFramework, err := framework.NewFrameworkFromConfig(
				restConfig,
				&framework.SSHConfig{
					Key:  mastermachinepk,
					User: "ec2-user",
				},
			)
			if err != nil {
				return err
			}

			clusterFramework.ErrNotExpected = func(err error) {
				if err != nil {
					glog.Fatal(err)
				}
			}

			clusterFramework.By = func(msg string) {
				glog.Info(msg)
			}

			clusterFramework.MachineControllerImage = "openshift/origin-aws-machine-controllers:v4.0.0"
			clusterFramework.MachineManagerImage = "openshift/origin-aws-machine-controllers:v4.0.0"
			clusterFramework.NodelinkControllerImage = "registry.svc.ci.openshift.org/openshift/origin-v4.0-2019-01-03-031244@sha256:152c0a4ea7cda1731e45af87e33909421dcde7a8fcf4e973cd098a8bae892c50"

			glog.Info("Waiting for all nodes to come up")
			err = clusterFramework.WaitForNodesToGetReady(1)
			if err != nil {
				return err
			}

			glog.Infof("Creating %q namespace", testNamespace.Name)
			if _, err := clusterFramework.KubeClient.CoreV1().Namespaces().Create(testNamespace); err != nil {
				return err
			}

			clusterFramework.DeployClusterAPIStack(testNamespace.Name, "")
			clusterFramework.CreateClusterAndWait(testCluster)
			createSecretAndWait(clusterFramework, awsCredentialsSecret)

			workerUserDataSecret, err := manifests.WorkerMachineUserDataSecret("workeruserdatasecret", testNamespace.Name, masterMachinePrivateIP)
			if err != nil {
				return err
			}

			createSecretAndWait(clusterFramework, workerUserDataSecret)
			workerMachineSetProviderConfig, err := testutils.WorkerMachineSetProviderConfig(awsCredentialsSecret.Name, workerUserDataSecret.Name, testCluster.Name)
			if err != nil {
				return err
			}
			workerMachineSet := manifests.WorkerMachineSet(testCluster.Name, testCluster.Namespace, workerMachineSetProviderConfig)
			clusterFramework.CreateMachineSetAndWait(workerMachineSet, acw)

			return nil
		},
	}

	cmd.PersistentFlags().StringP("manifests", "", "", "Directory with bootstrapping manifests")
	cmd.PersistentFlags().StringP("master-machine-private-key", "", "", "Private key file of the master machine to pull kubeconfig")
	return cmd
}

func cmdRun(binaryPath string, args ...string) ([]byte, error) {
	cmd := exec.Command(binaryPath, args...)
	return cmd.CombinedOutput()
}

func init() {
	// Add types to scheme
	clusterv1.AddToScheme(scheme.Scheme)

	rootCmd.PersistentFlags().StringP("machine", "m", "", "Machine manifest")
	rootCmd.PersistentFlags().StringP("cluster", "c", "", "Cluster manifest")
	rootCmd.PersistentFlags().StringP("aws-credentials", "a", "", "Secret manifest with aws credentials")
	rootCmd.PersistentFlags().StringP("userdata", "u", "", "User data manifest")
	cUser, err := user.Current()
	if err != nil {
		rootCmd.PersistentFlags().StringP("environment-id", "p", "", "Directory with bootstrapping manifests")
	} else {
		rootCmd.PersistentFlags().StringP("environment-id", "p", cUser.Username, "Machine prefix, by default set to the current user")
	}

	rootCmd.AddCommand(createCommand())

	rootCmd.AddCommand(deleteCommand())

	rootCmd.AddCommand(existsCommand())

	rootCmd.AddCommand(bootstrapCommand())

	flag.CommandLine.AddGoFlagSet(goflag.CommandLine)

	// the following line exists to make glog happy, for more information, see: https://github.com/kubernetes/kubernetes/issues/17162
	flag.CommandLine.Parse([]string{})
}

type manifestParams struct {
	ClusterID string
}

func readClusterResources(manifestParams *manifestParams, clusterLoc, machineLoc, awsCredentialSecretLoc, userDataLoc string) (*clusterv1.Cluster, *clusterv1.Machine, *apiv1.Secret, *apiv1.Secret, error) {
	var err error
	machine, err := readMachineManifest(manifestParams, machineLoc)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	cluster := &clusterv1.Cluster{}
	{
		bytes, err := ioutil.ReadFile(clusterLoc)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("cluster manifest %q: %v", clusterLoc, err)
		}

		if err = yaml.Unmarshal(bytes, &cluster); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("cluster manifest %q: %v", clusterLoc, err)
		}
	}

	var awsCredentialsSecret *apiv1.Secret
	if awsCredentialSecretLoc != "" {
		awsCredentialsSecret = &apiv1.Secret{}
		bytes, err := ioutil.ReadFile(awsCredentialSecretLoc)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("aws credentials manifest %q: %v", awsCredentialSecretLoc, err)
		}

		if err = yaml.Unmarshal(bytes, &awsCredentialsSecret); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("aws credentials manifest %q: %v", awsCredentialSecretLoc, err)
		}
	}

	var userDataSecret *apiv1.Secret
	if userDataLoc != "" {
		userDataSecret = &apiv1.Secret{}
		bytes, err := ioutil.ReadFile(userDataLoc)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("user data manifest %q: %v", userDataLoc, err)
		}

		if err = yaml.Unmarshal(bytes, &userDataSecret); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("user data manifest %q: %v", userDataLoc, err)
		}
	}

	return cluster, machine, awsCredentialsSecret, userDataSecret, nil
}

func checkFlags(cmd *cobra.Command) error {
	if cmd.Flag("cluster").Value.String() == "" {
		return fmt.Errorf("--%v/-%v flag is required", cmd.Flag("cluster").Name, cmd.Flag("cluster").Shorthand)
	}
	if cmd.Flag("machine").Value.String() == "" {
		return fmt.Errorf("--%v/-%v flag is required", cmd.Flag("machine").Name, cmd.Flag("machine").Shorthand)
	}
	return nil
}

func main() {
	err := rootCmd.Execute()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error occurred: %v\n", err)
		os.Exit(1)
	}
}
