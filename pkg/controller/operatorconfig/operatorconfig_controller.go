package operatorconfig

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/golang/glog"
	"github.com/openshift/ptp-operator/pkg/apply"
	"github.com/openshift/ptp-operator/pkg/names"
	"github.com/openshift/ptp-operator/pkg/render"
	ptpv1 "github.com/openshift/ptp-operator/pkg/apis/ptp/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	uns "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_operatorconfig")

const (
	ResyncPeriod = 5 * time.Minute
)

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new OperatorConfig Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileOperatorConfig{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("operatorconfig-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource OperatorConfig
	err = c.Watch(&source.Kind{Type: &ptpv1.OperatorConfig{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileOperatorConfig implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileOperatorConfig{}

// ReconcileOperatorConfig reconciles a OperatorConfig object
type ReconcileOperatorConfig struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a OperatorConfig object and makes changes based on the state read
// and what is in the OperatorConfig.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileOperatorConfig) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling OperatorConfig")

	// Fetch the OperatorConfig instance
	defaultCfg := &ptpv1.OperatorConfig{}
	err := r.client.Get(context.TODO(), types.NamespacedName{
		Name: names.DefaultOperatorConfigName, Namespace: names.Namespace}, defaultCfg)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			defaultCfg.SetNamespace(names.Namespace)
			defaultCfg.SetName(names.DefaultOperatorConfigName)
			defaultCfg.Spec = ptpv1.OperatorConfigSpec{
				DaemonNodeSelector: map[string]string{},
			}
			if err = r.client.Create(context.TODO(), defaultCfg); err != nil {
				reqLogger.Error(err, "failed to create default ptp config",
					"Namespace", names.Namespace, "Name", names.DefaultOperatorConfigName)
				return reconcile.Result{}, err
			}
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	nodeList := &corev1.NodeList{}
	err = r.client.List(context.TODO(), &client.ListOptions{}, nodeList)
	if err != nil {
		glog.Errorf("failed to list nodes")
		return reconcile.Result{}, err
	}

	if err = r.syncNodePtpDevice(nodeList); err != nil {
		return reconcile.Result{}, err
	}

	if err = r.syncLinuxptpDaemon(defaultCfg); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: ResyncPeriod}, nil
}

// syncLinuxptpDaemon synchronizes Linuxptp DaemonSet
func (r *ReconcileOperatorConfig) syncLinuxptpDaemon(defaultCfg *ptpv1.OperatorConfig) error {
        var err error
        objs := []*uns.Unstructured{}

        data := render.MakeRenderData()
        data.Data["Image"] = os.Getenv("LINUXPTP_DAEMON_IMAGE")
        data.Data["Namespace"] = names.Namespace
        data.Data["ReleaseVersion"] = os.Getenv("RELEASEVERSION")
        objs, err = render.RenderDir(filepath.Join(names.ManifestDir, "linuxptp"), &data)
        if err != nil {
                return fmt.Errorf("failed to render linuxptp daemon manifest: %v", err)
        }

        for _, obj := range objs {
                if err = controllerutil.SetControllerReference(defaultCfg, obj, r.scheme); err != nil {
                        return fmt.Errorf("failed to set owner reference: %v", err)
                }
                if err = apply.ApplyObject(context.TODO(), r.client, obj); err != nil {
                        return fmt.Errorf("failed to apply object %v with err: %v", obj, err)
                }
        }
        return nil
}

// syncNodePtpDevice synchronizes NodePtpDevice CR for each node
func (r *ReconcileOperatorConfig) syncNodePtpDevice(nodeList *corev1.NodeList) error {
	for _, node := range nodeList.Items {
		found := &ptpv1.NodePtpDevice{}
		err := r.client.Get(context.TODO(), types.NamespacedName{
			Namespace: names.Namespace, Name: node.Name}, found)
		if err != nil {
			if errors.IsNotFound(err) {
				ptpDev := &ptpv1.NodePtpDevice{}
				ptpDev.Name = node.Name
				ptpDev.Namespace = names.Namespace
				err = r.client.Create(context.TODO(), ptpDev)
				if err != nil {
					return fmt.Errorf("failed to create NodePtpDevice for node: %v", node.Name)
				}
				glog.Infof("create NodePtpDevice successfully for node: %v", node.Name)
			} else {
				return fmt.Errorf("failed to get NodePtpDevice for node: %v", node.Name)
			}
		} else {
			glog.Infof("NodePtpDevice exists for node: %v, skipping", node.Name)
		}
	}
	return nil
}