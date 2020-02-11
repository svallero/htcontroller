/*

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

package controllers

import (
	"context"
	//"net"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	htcondorv1 "htcontroller/api/v1"

	"github.com/pkg/errors"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	//apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// Global variable for cluster name
//var cluster_name string

// Generate random name
// needed by executors to get different hostnames
// (if hostname is not specified, name is not resolved)

// Role properties
type Role struct {
	// these are the features specific to each role,
	// the rest is common
	Name          string
	Replicas      int32
	ContainerPort []int32
	Args          []string
	Cpus          string
	Memory        string
}

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=htcondor.toinfn.it,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=htcondor.toinfn.it,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=deployments,verbs=get;watch;list;create;update;delete
// +kubebuilder:rbac:groups=htcondor.toinfn.it,resources=clusters/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups="";extensions,resources=events,verbs=get;list;watch;create;update;delete;patch
// +kubebuilder:rbac:groups="";extensions,resources=pods,verbs=get;list;watch;create;update;delete;patch
// +kubebuilder:rbac:groups=apps;extensions,resources=deployments,verbs=get;list;watch;create;update;delete;patch
// +kubebuilder:rbac:groups="";extensions,resources=services,verbs=get;list;watch;create;update;delete;patch

func (r *ClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("cluster", req.NamespacedName)
	// My logic here
	log.Info("fetching Cluster resource")
	cluster := htcondorv1.Cluster{}

	if err := r.Client.Get(ctx, req.NamespacedName, &cluster); err != nil {
		log.Error(err, "failed to get Cluster resource")
		// Ignore NotFound errors as they will be retried automatically
		// if the resource is created in future.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	//var err_tmp error
	//cluster_name, err_tmp = SetClusterName()
	//log.Error(err_tmp, "********* SARA *********")

	// Initial role states
	master_ready := false
	submitter_ready := false
	// Initial number of executors
	// (it's zero if Master and Submitter are not ready)
	num_executors := int32(0)

	// Initialize cluster status
	cluster.Status.MasterReady = master_ready
	cluster.Status.SubmitterReady = submitter_ready
	cluster.Status.ExecutorsReady = num_executors

	// Namespace
	namespace := cluster.Namespace
	// Dedicated scheduler
	//dedicated_scheduler := cluster.Name + "-submitter." + namespace + ".svc." + cluster.Spec.ClusterName
	//master_fqdn := cluster.Name + "-master." + namespace + ".svc." + cluster.Spec.ClusterName

	// *** MASTER ***
	submitter_name := cluster.Name + "-submitter"
	master_role := Role{
		Name:          "master",
		Replicas:      1,
		ContainerPort: []int32{9618},
		//Args:          []string{"-m", "-S", cluster.Spec.Secret, "-C", cluster.Spec.ExecutorCpus, "-D", submitter_name},
		Args:   []string{"-m", "-S", cluster.Spec.Secret},
		Cpus:   cluster.Spec.MasterCpus,
		Memory: cluster.Spec.MasterMemory,
	}

	// Name and namespace
	master_name := cluster.Name + "-" + master_role.Name

	// Service
	var master_service core.Service
	master_service.Name = master_name
	master_service.Namespace = namespace

	// CreateOrUpdate service
	_, err := ctrl.CreateOrUpdate(ctx, r, &master_service, func() error {
		ModifyService(cluster, master_role, &master_service)
		return controllerutil.SetControllerReference(&cluster, &master_service, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "unable to CreateOrUpdate Service for role Master")
	}

	// Deployment
	var master_deployment apps.Deployment
	master_deployment.Name = master_name
	master_deployment.Namespace = namespace

	// CreateOrUpdate deployment
	_, err = ctrl.CreateOrUpdate(ctx, r, &master_deployment, func() error {
		ModifyDeployment(cluster, master_role, &master_deployment)
		return controllerutil.SetControllerReference(&cluster, &master_deployment, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "unable to CreateOrUpdate Deployment for role Master")
	}

	// Pod status
	log.Info("Checking Master readyness...")
	if ready := r.CheckRoleReady(ctx, log, master_name, namespace, master_role); ready == 0 {
		// if master is not ready, we update the cluster status and exit
		log.Info("Master is not ready!")
		//if err := r.UpdateClusterStatus(ctx, cluster, log); err != nil {
		//	return ctrl.Result{}, err
		//}
		return ctrl.Result{}, r.UpdateClusterStatus(ctx, cluster, log)
	}
	master_ready = true

	// *** SUBMITTER ***
	submitter_role := Role{
		Name:          "submitter",
		Replicas:      1,
		ContainerPort: []int32{9618, 22},
		//Args:          []string{"-s", master_name, "-S", cluster.Spec.Secret, "-C", cluster.Spec.ExecutorCpus, "-D", submitter_name},
		Args:   []string{"-s", master_name, "-S", cluster.Spec.Secret},
		Cpus:   cluster.Spec.SubmitterCpus,
		Memory: cluster.Spec.SubmitterMemory,
	}

	// Service
	//submitter_name := cluster.Name + "-" + submitter_role.Name
	var submitter_service core.Service
	submitter_service.Name = submitter_name
	submitter_service.Namespace = namespace

	// CreateOrUpdate service
	_, err = ctrl.CreateOrUpdate(ctx, r, &submitter_service, func() error {
		ModifyService(cluster, submitter_role, &submitter_service)
		return controllerutil.SetControllerReference(&cluster, &submitter_service, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "unable to CreateOrUpdate Service for role Submitter")
	}

	// Deployment
	var submitter_deployment apps.Deployment
	submitter_deployment.Name = submitter_name
	submitter_deployment.Namespace = namespace

	// CreateOrUpdate deployment
	_, err = ctrl.CreateOrUpdate(ctx, r, &submitter_deployment, func() error {
		ModifyDeployment(cluster, submitter_role, &submitter_deployment)
		return controllerutil.SetControllerReference(&cluster, &submitter_deployment, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "unable to CreateOrUpdate Deployment for role Submitter")
	}

	// Pod status
	log.Info("Checking Submitter readyness...")
	if ready := r.CheckRoleReady(ctx, log, submitter_name, namespace, submitter_role); ready == 0 {
		// if submitter is not ready, we update the cluster status and exit
		log.Info("Submitter is not ready!")
		cluster.Status.MasterReady = master_ready
		//if err := r.UpdateClusterStatus(ctx, cluster, log); err != nil {
		//	return ctrl.Result{}, err
		//}
		return ctrl.Result{}, r.UpdateClusterStatus(ctx, cluster, log)
	}
	submitter_ready = true

	// *** EXECUTORS ***
	replicas := cluster.Spec.RequestedExecutors
	if *replicas < cluster.Spec.MinExecutors {
		*replicas = cluster.Spec.MinExecutors
	}
	if *replicas > cluster.Spec.MaxExecutors {
		*replicas = cluster.Spec.MaxExecutors
	}

	executor_role := Role{
		Name:          "executor",
		Replicas:      *replicas,
		ContainerPort: []int32{22},
		Args:          []string{"-e", master_name, "-S", cluster.Spec.Secret, "-C", cluster.Spec.ExecutorCpus, "-D", submitter_name},
		Cpus:          cluster.Spec.ExecutorCpus,
		Memory:        cluster.Spec.ExecutorMemory,
	}

	// Name and namespace
	executor_name := cluster.Name + "-" + executor_role.Name

	// Deployment
	var executor_deployment apps.Deployment
	executor_deployment.Name = executor_name
	executor_deployment.Namespace = namespace

	// CreateOrUpdate deployment
	_, err = ctrl.CreateOrUpdate(ctx, r, &executor_deployment, func() error {
		ModifyDeployment(cluster, executor_role, &executor_deployment)
		return controllerutil.SetControllerReference(&cluster, &executor_deployment, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "unable to CreateOrUpdate Deployment for role Executor")
	}

	// Pod status
	log.Info("Checking Executors readyness...")
	if ready := r.CheckRoleReady(ctx, log, executor_name, namespace, executor_role); ready == 0 {
		// if execuotrs are not ready, we update the cluster status and exit
		log.Info("Executors are not ready!")
		//if err := r.UpdateClusterStatus(ctx, cluster, log); err != nil {
		//	return ctrl.Result{}, err
		//}
	} else {
		num_executors = ready
	}

	// Update the cluster status
	cluster.Status.MasterReady = master_ready
	cluster.Status.SubmitterReady = submitter_ready
	cluster.Status.ExecutorsReady = num_executors
	return ctrl.Result{}, r.UpdateClusterStatus(ctx, cluster, log)
	//if err := r.UpdateClusterStatus(ctx, cluster, log); err != nil {
	//return ctrl.Result{}, err
	//}

	//return ctrl.Result{}, nil
}

func (r *ClusterReconciler) UpdateClusterStatus(ctx context.Context, cluster htcondorv1.Cluster, log logr.Logger) error {

	log.Info("Updating cluster status")
	err := r.Update(ctx, &cluster)
	if err != nil {
		return err
	}

	// register event
	r.Recorder.Event(&cluster, core.EventTypeNormal, "Updated", "Cluster status updated")

	return nil

}

func ModifyService(cluster htcondorv1.Cluster, role Role, service *core.Service) {
	// LABELS
	labels := map[string]string{"app": service.Name}
	service.Spec.Selector = labels
	var ports []core.ServicePort
	for i, p := range role.ContainerPort {
		port := core.ServicePort{
			Port: p,
			Name: "port" + strconv.FormatInt(int64(i), 10),
		}
		ports = append(ports, port)
		//ports[i] = port
	}
	service.Spec.Ports = ports
	service.Spec.ClusterIP = "None"

}

func ModifyDeployment(cluster htcondorv1.Cluster, role Role, deployment *apps.Deployment) {
	// LABELS
	labels := map[string]string{"app": deployment.Name}
	if deployment.Labels == nil {
		deployment.Labels = make(map[string]string)
	}
	for k, v := range labels {
		deployment.Labels[k] = v
	}
	deployment.Spec.Template.Labels = labels
	deployment.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: labels,
	}

	// REPLICAS
	//var replicas int32 = 1
	//deployment.Spec.Replicas = &replicas
	deployment.Spec.Replicas = &role.Replicas

	// STRATEGY
	deployment.Spec.Strategy = apps.DeploymentStrategy{Type: "Recreate"}

	// CONTAINER
	templateSpec := &deployment.Spec.Template.Spec
	//if role.Name != "executor" {
	//	templateSpec.Hostname = deployment.Name
	//}
	templateSpec.Hostname = deployment.Name

	if len(templateSpec.Containers) == 0 {
		templateSpec.Containers = make([]core.Container, 1)
	}
	container := &templateSpec.Containers[0]

	container.Name = deployment.Name
	container.Image = cluster.Spec.ImageName
	container.ImagePullPolicy = "Always"

	var cports []core.ContainerPort
	for i, p := range role.ContainerPort {
		port := core.ContainerPort{
			ContainerPort: p,
			Name:          "port" + strconv.FormatInt(int64(i), 10),
		}
		cports = append(cports, port)
	}

	container.Ports = cports

	container.Args = role.Args

	container.Resources = core.ResourceRequirements{
		Limits: core.ResourceList{
			core.ResourceCPU:    resource.MustParse(role.Cpus),
			core.ResourceMemory: resource.MustParse(role.Memory),
		},
	}

	// Same volume mounts on all roles
	var vmounts []core.VolumeMount
	var volumes []core.Volume
	for mount, claim := range cluster.Spec.VolumeMounts {
		name := strings.Replace(mount, "/", "", -1)
		// container volume mounts
		vmount := core.VolumeMount{
			Name:      name,
			MountPath: mount,
		}
		vmounts = append(vmounts, vmount)
		// template spec volumes
		volume_source := core.VolumeSource{
			PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{
				ClaimName: claim,
			},
		}
		volume := core.Volume{
			Name:         name,
			VolumeSource: volume_source,
		}
		volumes = append(volumes, volume)
	}
	container.VolumeMounts = vmounts
	templateSpec.Volumes = volumes

	// health-checks
	probe := core.Probe{
		Handler: core.Handler{
			HTTPGet: &core.HTTPGetAction{
				Path: "/health",
				Port: intstr.IntOrString{IntVal: 5000},
			},
		},
		InitialDelaySeconds: 3,
		PeriodSeconds:       10,
	}

	// liveness (for restarts)
	container.LivenessProbe = &probe
	// readyness probe (for status ready)
	container.ReadinessProbe = &probe

}

func (r *ClusterReconciler) CheckRoleReady(ctx context.Context, log logr.Logger, name string, namespace string, role Role) int32 {
	list := core.PodList{}
	err := r.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{"app": name})
	if err != nil {
		log.Error(err, "failed to get Pods  for "+role.Name+"  role")
		return 0
	}

	var role_ready int32 = 0
	for _, pod := range list.Items {
		log.Info("Pod name: " + pod.Name)
		//log.Info("Pod status: " + pod.Status.String())
		contready := pod.Status.ContainerStatuses
		for _, status := range contready {
			log.Info("Pod ready: " + strconv.FormatBool(status.Ready))
			if status.Ready {
				role_ready += 1
			}
		}
	}

	return role_ready
}

//func SetClusterName() (string, error) {

//	var name string
//	apiSvc := "kubernetes.default.svc"
//	cname, err := net.LookupCNAME(apiSvc)
//	if err != nil {
//		return "pluto", err
//	}
//	name = strings.TrimPrefix(cname, apiSvc)
//	name = strings.TrimSuffix(name, ".")
//	return name, err
//}

func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {

	//cluster_name, err = SetClusterName()

	return ctrl.NewControllerManagedBy(mgr).
		For(&htcondorv1.Cluster{}).
		Watches(&source.Kind{Type: &apps.Deployment{}},
			&handler.EnqueueRequestForOwner{
				IsController: true,
				OwnerType:    &htcondorv1.Cluster{},
			}).
		Complete(r)
}
