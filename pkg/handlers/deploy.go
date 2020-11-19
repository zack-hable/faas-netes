// Copyright (c) Alex Ellis 2017. All rights reserved.
// Copyright 2020 OpenFaaS Author(s)
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"math"
	"errors"

	"github.com/openfaas/faas-netes/pkg/k8s"

	types "github.com/openfaas/faas-provider/types"
	appsv1 "k8s.io/api/apps/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apiv1 "k8s.io/api/core/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// initialReplicasCount how many replicas to start of creating for a function
const initialReplicasCount = 1
const initialPodPriority = 5
const initialPodTaskType = CPU_BOUND
const initialBalanceMode = NONE

type TaskType int32
const (
	CPU_BOUND TaskType = iota
	IO_BOUND
)

type BalanceMode int32
const (
	NONE BalanceMode = iota
	LEAST_LOADED
)

// MakeDeployHandler creates a handler to create new functions in the cluster
func MakeDeployHandler(functionNamespace string, factory k8s.FunctionFactory) http.HandlerFunc {
	secrets := k8s.NewSecretsClient(factory.Client)

	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if r.Body != nil {
			defer r.Body.Close()
		}

		body, _ := ioutil.ReadAll(r.Body)

		request := types.FunctionDeployment{}
		err := json.Unmarshal(body, &request)
		if err != nil {
			wrappedErr := fmt.Errorf("failed to unmarshal request: %s", err.Error())
			http.Error(w, wrappedErr.Error(), http.StatusBadRequest)
			return
		}

		if err := ValidateDeployRequest(&request); err != nil {
			wrappedErr := fmt.Errorf("validation failed: %s", err.Error())
			http.Error(w, wrappedErr.Error(), http.StatusBadRequest)
			return
		}

		namespace := functionNamespace
		if len(request.Namespace) > 0 {
			namespace = request.Namespace
		}

		existingSecrets, err := secrets.GetSecrets(namespace, request.Secrets)
		if err != nil {
			wrappedErr := fmt.Errorf("unable to fetch secrets: %s", err.Error())
			http.Error(w, wrappedErr.Error(), http.StatusBadRequest)
			return
		}

		deploymentSpec, specErr := makeDeploymentSpec(request, existingSecrets, factory)

		var profileList []k8s.Profile
		if request.Annotations != nil {
			profileNamespace := factory.Config.ProfilesNamespace
			profileList, err = factory.GetProfiles(ctx, profileNamespace, *request.Annotations)
			if err != nil {
				wrappedErr := fmt.Errorf("failed create Deployment spec: %s", err.Error())
				log.Println(wrappedErr)
				http.Error(w, wrappedErr.Error(), http.StatusBadRequest)
				return
			}
		}
		for _, profile := range profileList {
			factory.ApplyProfile(profile, deploymentSpec)
		}

		if specErr != nil {
			wrappedErr := fmt.Errorf("failed create Deployment spec: %s", specErr.Error())
			log.Println(wrappedErr)
			http.Error(w, wrappedErr.Error(), http.StatusBadRequest)
			return
		}

		deploy := factory.Client.AppsV1().Deployments(namespace)

		_, err = deploy.Create(context.TODO(), deploymentSpec, metav1.CreateOptions{})
		if err != nil {
			wrappedErr := fmt.Errorf("unable create Deployment: %s", err.Error())
			log.Println(wrappedErr)
			http.Error(w, wrappedErr.Error(), http.StatusInternalServerError)
			// cleanup priority class we made
			deployPriorityClass := factory.Client.SchedulingV1().PriorityClasses()
			deployPriorityClass.Delete(context.TODO(), request.Service, metav1.DeleteOptions{})
			return
		}

		log.Printf("Deployment created: %s.%s\n", request.Service, namespace)

		service := factory.Client.CoreV1().Services(namespace)
		serviceSpec := makeServiceSpec(request, factory)
		_, err = service.Create(context.TODO(), serviceSpec, metav1.CreateOptions{})

		if err != nil {
			wrappedErr := fmt.Errorf("failed create Service: %s", err.Error())
			log.Println(wrappedErr)
			http.Error(w, wrappedErr.Error(), http.StatusBadRequest)
			return
		}

		log.Printf("Service created: %s.%s\n", request.Service, namespace)

		w.WriteHeader(http.StatusAccepted)
	}
}

func makeDeploymentSpec(request types.FunctionDeployment, existingSecrets map[string]*apiv1.Secret, factory k8s.FunctionFactory) (*appsv1.Deployment, error) {
	envVars := buildEnvVars(&request)

	initialReplicas := int32p(initialReplicasCount)
	podPriority := int32p(initialPodPriority)
	// assume task type is cpu bound
	podTaskType := new(TaskType)
	*podTaskType = initialPodTaskType
	podBalanceMode := new(BalanceMode)
	*podBalanceMode = initialBalanceMode
	labels := map[string]string{
		"faas_function": request.Service,
	}

	if request.Labels != nil {
		if min := getMinReplicaCount(*request.Labels); min != nil {
			initialReplicas = min
		}
		if priority := getPodPriority(*request.Labels); priority != nil {
			podPriority = priority
		}
		if taskType := getPodTaskType(*request.Labels); taskType != nil {
			*podTaskType = *taskType
		}
		if balanceMode := getBalancerMode(*request.Labels); balanceMode != nil {
			*podBalanceMode = *balanceMode
		}
		for k, v := range *request.Labels {
			labels[k] = v
		}
	}

	nodeSelector := createSelector(request.Constraints)

	resources, resourceErr := createResources(request)

	if resourceErr != nil {
		return nil, resourceErr
	}

	var imagePullPolicy apiv1.PullPolicy
	switch factory.Config.ImagePullPolicy {
	case "Never":
		imagePullPolicy = apiv1.PullNever
	case "IfNotPresent":
		imagePullPolicy = apiv1.PullIfNotPresent
	default:
		imagePullPolicy = apiv1.PullAlways
	}

	annotations := buildAnnotations(request)

	var serviceAccount string

	if request.Annotations != nil {
		annotations := *request.Annotations
		if val, ok := annotations["com.openfaas.serviceaccount"]; ok && len(val) > 0 {
			serviceAccount = val
		}
	}

	probes, err := factory.MakeProbes(request)
	if err != nil {
		return nil, err
	}

	enableServiceLinks := false

	if *podBalanceMode == LEAST_LOADED {
		// construct image of nodes and assigned pods to them to determine next assignment
		scheduleTable := make(map[string]map[TaskType][]apiv1.Pod)

		nodes, err := factory.Client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			log.Println("Error while getting nodes")
			log.Println(err)
		} else {
			for _, node := range nodes.Items {
				nodeLabel, exists := node.Labels["node"]
				if !exists {
					log.Println("No node label associated with node...")
				} else {
					// create subtables for different types of functions
					scheduleTable[nodeLabel] = make(map[TaskType][]apiv1.Pod)
					scheduleTable[nodeLabel][CPU_BOUND] = make([]apiv1.Pod, 0)
					scheduleTable[nodeLabel][IO_BOUND] = make([]apiv1.Pod, 0)
				}
			}
		}

		log.Println(len(scheduleTable), " nodes available to schedule to")
		// if no nodes in schedule table then throw an error (user needs to label at least one node)
		if len(scheduleTable) == 0 {
			err = errors.New("No nodes are available to balance to.  Ensure at least one node is labeled with 'node=X' (where X is unique).")
			return nil, err
		}

		// get current pods and their assignments
		pods, err := factory.Client.CoreV1().Pods("openfaas-fn").List(context.TODO(), metav1.ListOptions{})
		//log.Println("Pod Task Type", *podTaskType)
		//log.Println(pods.Items)
		if err != nil {
			log.Println("Error while getting pods")
			log.Println(err)
		} else {
			for _, pod := range pods.Items {
				//log.Println("Pod Spec", pod.Spec)
				//log.Println("Node Selector", pod.Spec.NodeSelector)
				nodeLabel, nodeExists := pod.Spec.NodeSelector["node"]
				taskTypeLabel, taskTypeExists := pod.Labels["task_type"]
				if !nodeExists {
					log.Println("No node label associated with pod...")
				} else if !taskTypeExists {
					log.Println("No task type label associated with pod...")
				} else {
					taskTypeInt, err := strconv.Atoi(taskTypeLabel)
					if err != nil {
						log.Println("Error converting task type label to integer")
						log.Println(err)
					} else {
						taskTypeEnum := TaskType(taskTypeInt)
						scheduleTable[nodeLabel][taskTypeEnum] = append(scheduleTable[nodeLabel][taskTypeEnum], pod)
					}
				}
			}
		}

		// perform simple min loaded assignment of given type
		// assume server 1 if we find nothing
		minLoadedServer := "1"
		minLoaderServerCount := math.MaxInt64
		// iterate over table to display some basic stats
		log.Println("Node# - TaskType - #Pods")
		for nodeName, taskTypes := range scheduleTable {
			for taskType, nodeTaskPods := range taskTypes {
				log.Println(nodeName, " - ", taskType, " - ", len(nodeTaskPods))
				// check if matches our type and is lowest
				if len(nodeTaskPods) < minLoaderServerCount && taskType == *podTaskType {
					minLoaderServerCount = len(nodeTaskPods)
					minLoadedServer = nodeName
				}
			}
		}

		nodeSelector["node"] = minLoadedServer
		log.Println("Service assigned to node ", minLoadedServer)
	}

	// save task type
	labels["task_type"] = strconv.Itoa(int(*podTaskType))

	// handle priority scheduling
	deployPriorityClass := factory.Client.SchedulingV1().PriorityClasses()

	_, getPriorityClassErr := deployPriorityClass.Get(context.TODO(), request.Service, metav1.GetOptions{})
	if getPriorityClassErr == nil {
		// delete it
		deployPriorityClass.Delete(context.TODO(), request.Service, metav1.DeleteOptions{})
	}

	preemptionPolicy := apiv1.PreemptLowerPriority
	priorityClassSpec := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: request.Service,
		},
		Value: *podPriority,
		GlobalDefault: false,
		PreemptionPolicy: &preemptionPolicy,
	}

	podPriorityClass, err := deployPriorityClass.Create(context.TODO(), priorityClassSpec, metav1.CreateOptions{})
	if err != nil {
		wrappedErr := fmt.Errorf("unable create PriorityClass: %s", err.Error())
		log.Println(wrappedErr)
		//http.Error(w, wrappedErr.Error(), http.StatusInternalServerError)
		return nil, err
	}

	deploymentSpec := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        request.Service,
			Annotations: annotations,
			Labels: map[string]string{
				"faas_function": request.Service,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"faas_function": request.Service,
				},
			},
			Replicas: initialReplicas,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: int32(0),
					},
					MaxSurge: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: int32(1),
					},
				},
			},
			RevisionHistoryLimit: int32p(10),
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:        request.Service,
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: apiv1.PodSpec{
					PriorityClassName: podPriorityClass.ObjectMeta.Name,
					NodeSelector: nodeSelector,
					Containers: []apiv1.Container{
						{
							Name:  request.Service,
							Image: request.Image,
							Ports: []apiv1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: factory.Config.RuntimeHTTPPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Env:             envVars,
							Resources:       *resources,
							ImagePullPolicy: imagePullPolicy,
							LivenessProbe:   probes.Liveness,
							ReadinessProbe:  probes.Readiness,
							SecurityContext: &corev1.SecurityContext{
								ReadOnlyRootFilesystem: &request.ReadOnlyRootFilesystem,
							},
						},
					},
					ServiceAccountName: serviceAccount,
					RestartPolicy:      corev1.RestartPolicyAlways,
					DNSPolicy:          corev1.DNSClusterFirst,
					// EnableServiceLinks injects ENV vars about every other service within
					// the namespace.
					EnableServiceLinks: &enableServiceLinks,
				},
			},
		},
	}

	factory.ConfigureReadOnlyRootFilesystem(request, deploymentSpec)
	factory.ConfigureContainerUserID(deploymentSpec)

	if err := factory.ConfigureSecrets(request, deploymentSpec, existingSecrets); err != nil {
		return nil, err
	}

	return deploymentSpec, nil
}

func makeServiceSpec(request types.FunctionDeployment, factory k8s.FunctionFactory) *corev1.Service {

	serviceSpec := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        request.Service,
			Annotations: buildAnnotations(request),
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"faas_function": request.Service,
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Protocol: corev1.ProtocolTCP,
					Port:     factory.Config.RuntimeHTTPPort,
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: factory.Config.RuntimeHTTPPort,
					},
				},
			},
		},
	}

	return serviceSpec
}

func buildAnnotations(request types.FunctionDeployment) map[string]string {
	var annotations map[string]string
	if request.Annotations != nil {
		annotations = *request.Annotations
	} else {
		annotations = map[string]string{}
	}

	if _, ok := annotations["prometheus.io.scrape"]; !ok {
		annotations["prometheus.io.scrape"] = "false"
	}
	return annotations
}

func buildEnvVars(request *types.FunctionDeployment) []corev1.EnvVar {
	envVars := []corev1.EnvVar{}

	if len(request.EnvProcess) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  k8s.EnvProcessName,
			Value: request.EnvProcess,
		})
	}

	for k, v := range request.EnvVars {
		envVars = append(envVars, corev1.EnvVar{
			Name:  k,
			Value: v,
		})
	}

	sort.SliceStable(envVars, func(i, j int) bool {
		return strings.Compare(envVars[i].Name, envVars[j].Name) == -1
	})

	return envVars
}

func int32p(i int32) *int32 {
	return &i
}

func createSelector(constraints []string) map[string]string {
	selector := make(map[string]string)

	if len(constraints) > 0 {
		for _, constraint := range constraints {
			parts := strings.Split(constraint, "=")

			if len(parts) == 2 {
				selector[parts[0]] = parts[1]
			}
		}
	}

	return selector
}

func createResources(request types.FunctionDeployment) (*apiv1.ResourceRequirements, error) {
	resources := &apiv1.ResourceRequirements{
		Limits:   apiv1.ResourceList{},
		Requests: apiv1.ResourceList{},
	}

	// Set Memory limits
	if request.Limits != nil && len(request.Limits.Memory) > 0 {
		qty, err := resource.ParseQuantity(request.Limits.Memory)
		if err != nil {
			return resources, err
		}
		resources.Limits[apiv1.ResourceMemory] = qty
	}

	if request.Requests != nil && len(request.Requests.Memory) > 0 {
		qty, err := resource.ParseQuantity(request.Requests.Memory)
		if err != nil {
			return resources, err
		}
		resources.Requests[apiv1.ResourceMemory] = qty
	}

	// Set CPU limits
	if request.Limits != nil && len(request.Limits.CPU) > 0 {
		qty, err := resource.ParseQuantity(request.Limits.CPU)
		if err != nil {
			return resources, err
		}
		resources.Limits[apiv1.ResourceCPU] = qty
	}

	if request.Requests != nil && len(request.Requests.CPU) > 0 {
		qty, err := resource.ParseQuantity(request.Requests.CPU)
		if err != nil {
			return resources, err
		}
		resources.Requests[apiv1.ResourceCPU] = qty
	}

	return resources, nil
}

func getMinReplicaCount(labels map[string]string) *int32 {
	if value, exists := labels["com.openfaas.scale.min"]; exists {
		minReplicas, err := strconv.Atoi(value)
		if err == nil && minReplicas > 0 {
			return int32p(int32(minReplicas))
		}

		log.Println(err)
	}

	return nil
}

func getPodPriority(labels map[string]string) *int32 {
	if value, exists := labels["cs2510.priority"]; exists {
		podPriority, err := strconv.Atoi(value)
		if err == nil && podPriority > 0 {
			return int32p(int32(podPriority))
		}

		log.Println(err)
	}

	return nil
}

func getPodTaskType(labels map[string]string) *TaskType {
	var res *TaskType = nil
	if value, exists := labels["cs2510.task.type"]; exists {
		res = new(TaskType)
		if strings.ToUpper(value) == "IO" {
			*res = IO_BOUND
		} else {
			*res = CPU_BOUND
		}
	}
	return res
}

func getBalancerMode(labels map[string]string) *BalanceMode {
	var res *BalanceMode = nil
	if value, exists := labels["cs2510.balancer"]; exists {
		res = new(BalanceMode)
		if strings.ToUpper(value) == "LEAST_LOADED" {
			*res = LEAST_LOADED
		} else {
			*res = NONE
		}
	}
	return res
}
