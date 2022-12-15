/*
Copyright 2022.

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
	"fmt"
	"github.com/pkg/errors"
	"net/http"
	"strconv"
	"time"

	"github.com/api7/cloud-go-sdk"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	_publishedServiceAnnotation                 = "api7.cloud/published-service"
	_publishedCanaryServiceAnnotation           = "api7.cloud/published-canary-service"
	_publishedServiceCanaryPercentageAnnotation = "api7.cloud/published-service-canary-percentage"
)

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	Token string
	Addr  string

	sdk          cloud.Interface
	controlPlane *cloud.ControlPlane
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=services/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Service object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var svc corev1.Service

	err := r.Get(ctx, req.NamespacedName, &svc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	return r.reconcile(ctx, &svc)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	sdk, err := cloud.NewInterface(&cloud.Options{
		ServerAddr:  r.Addr,
		Token:       r.Token,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return errors.Wrap(err, "failed to init api7 cloud sdk")
	}
	r.sdk = sdk
	if err = r.confirmControlPlane(); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Complete(r)
}

func (r *ServiceReconciler) reconcile(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	appName, ok := svc.Annotations[_publishedServiceAnnotation]
	if !ok {
		// missing "api7.cloud/published-service" annotation.
		return ctrl.Result{}, nil
	}

	app, err := r.findOrCreateApplication(ctx, svc, appName)
	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	canary := svc.Annotations[_publishedCanaryServiceAnnotation]
	if canary == "true" {
		var percentage int
		value, ok := svc.Annotations[_publishedServiceCanaryPercentageAnnotation]
		if ok {
			percentage, err = strconv.Atoi(value)
			if err != nil {
				percentage = 10
			}
			if percentage > 100 {
				percentage = 100
			}
		} else {
			percentage = 10
		}

		if err := r.createOrUpdateCanaryRelease(ctx, app, svc, percentage); err != nil {
			return ctrl.Result{Requeue: true}, err
		}
	} else {
		if err := r.pauseAllCanaryReleaseRules(ctx, app); err != nil {
			return ctrl.Result{Requeue: true}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *ServiceReconciler) confirmControlPlane() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	me, err := r.sdk.Me(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to fetch user self information")
	}
	iter, err := r.sdk.ListControlPlanes(ctx, &cloud.ResourceListOptions{
		Organization: &cloud.Organization{
			// Use the first organization
			ID: me.OrgIDs[0],
		},
	})
	if err != nil {
		return errors.Wrap(err, "failed to get control plane iterator")
	}
	// Only use the first control plane.
	cp, err := iter.Next()
	if err != nil {
		return errors.Wrap(err, "failed to get control plane")
	}
	r.controlPlane = cp
	return nil
}

func (r *ServiceReconciler) findOrCreateApplication(ctx context.Context, svc *corev1.Service, appName string) (*cloud.Application, error) {
	iter, err := r.sdk.ListApplications(ctx, &cloud.ResourceListOptions{
		ControlPlane: &cloud.ControlPlane{
			ID: r.controlPlane.ID,
		},
		Filter: &cloud.Filter{
			Search: appName,
		},
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list applications")
	}
	for {
		app, err := iter.Next()
		if err != nil {
			return nil, errors.Wrap(err, "failed to iterate applications")
		}
		if app == nil {
			break
		}
		if app.Name == appName {
			return app, nil
		}
	}

	// Application was not created yet.
	pathPrefix := "/"

	upstreamVersion := fmt.Sprintf("%s/%s", svc.Namespace, svc.Name)

	app, err := r.sdk.CreateApplication(ctx, &cloud.Application{
		ApplicationSpec: cloud.ApplicationSpec{
			Name:        appName,
			Description: fmt.Sprintf("This application is for the service %s/%s", svc.Namespace, svc.Name),
			Labels:      []string{"canary-release-controller"},
			Protocols:   []string{cloud.ProtocolHTTP},
			PathPrefix:  pathPrefix,
			Hosts:       []string{appName},
			Upstreams: []cloud.UpstreamAndVersion{
				{
					Upstream: cloud.Upstream{
						Scheme: cloud.UpstreamSchemeHTTP,
						LBType: cloud.LoadBalanceRoundRobin,
						Targets: []cloud.UpstreamTarget{
							{
								Host: fmt.Sprintf("%s.%s", svc.Name, svc.Namespace),
								// TODO: handle multiple ports.
								Port:   int(svc.Spec.Ports[0].Port),
								Weight: 100,
							},
						},
					},
					Version: upstreamVersion,
				},
			},
			DefaultUpstreamVersion: upstreamVersion,
			Active:                 cloud.ActiveStatus,
		},
	}, &cloud.ResourceCreateOptions{
		ControlPlane: &cloud.ControlPlane{
			ID: r.controlPlane.ID,
		},
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create application",
			"service name", svc.Name,
			"service namespace", svc.Namespace,
		)
		return nil, err
	}

	_, err = r.sdk.CreateAPI(ctx, &cloud.API{
		APISpec: cloud.APISpec{
			Name:    "anything",
			Methods: []string{http.MethodGet, http.MethodPost},
			Paths: []cloud.APIPath{
				{
					Path:     "/",
					PathType: cloud.PathPrefixMatch,
				},
			},
			StripPathPrefix: true,
			Active:          cloud.ActiveStatus,
			Type:            cloud.APITypeRest,
		},
	}, &cloud.ResourceCreateOptions{
		Application: app,
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create api",
			"service name", svc.Name,
			"service namespace", svc.Namespace,
		)
	}
	return app, nil
}

func (r *ServiceReconciler) pauseAllCanaryReleaseRules(ctx context.Context, app *cloud.Application) error {
	iter, err := r.sdk.ListCanaryReleases(ctx, &cloud.ResourceListOptions{
		Application: &cloud.Application{
			ID: app.ID,
		},
	})
	if err != nil {
		return errors.Wrap(err, "failed to get canary release iterator")
	}
	for {
		rule, err := iter.Next()
		if err != nil {
			return errors.Wrap(err, "failed to iterate canary release rule")
		}
		if rule == nil {
			return nil
		}

		if rule.State == cloud.CanaryReleaseStateInProgress {
			_, err := r.sdk.PauseCanaryRelease(ctx, rule, &cloud.ResourceUpdateOptions{
				Application: &cloud.Application{
					ID: app.ID,
				},
			})
			if err != nil {
				return errors.Wrap(err, "failed to pause canary release")
			}
		}
	}
}

func (r *ServiceReconciler) createOrUpdateCanaryRelease(ctx context.Context, app *cloud.Application, svc *corev1.Service, percentage int) error {
	ruleName := fmt.Sprintf("canary-release-%s/%s:%d", svc.Namespace, svc.Name, svc.Spec.Ports[0].Port)
	iter, err := r.sdk.ListCanaryReleases(ctx, &cloud.ResourceListOptions{
		Application: &cloud.Application{
			ID: app.ID,
		},
		Filter: &cloud.Filter{
			Search: ruleName,
		},
	})
	if err != nil {
		return errors.Wrap(err, "failed to get canary release iterator")
	}
	for {
		rule, err := iter.Next()
		if err != nil {
			return errors.Wrap(err, "failed to iterate canary release rule")
		}
		if rule == nil {
			break
		}

		if rule.Name == ruleName {
			rule.Percent = percentage
			if rule.Percent == 100 {
				_, err = r.sdk.FinishCanaryRelease(ctx, rule, &cloud.ResourceUpdateOptions{
					Application: &cloud.Application{
						ID: app.ID,
					},
				})
				if err != nil {
					return errors.Wrap(err, "failed to finish canary release")
				}
			} else {
				_, err = r.sdk.UpdateCanaryRelease(ctx, rule, &cloud.ResourceUpdateOptions{
					Application: &cloud.Application{
						ID: app.ID,
					},
				})
				if err != nil {
					return errors.Wrap(err, "failed to update canary release")
				}
			}

			return nil
		}
	}

	// Canary release doesn't exist.
	upstreamVersion := fmt.Sprintf("%s/%s", svc.Namespace, svc.Name)

	app.Upstreams = []cloud.UpstreamAndVersion{
		app.Upstreams[0],
		{
			Upstream: cloud.Upstream{
				Scheme: cloud.UpstreamSchemeHTTP,
				LBType: cloud.LoadBalanceRoundRobin,
				Targets: []cloud.UpstreamTarget{
					{
						Host:   fmt.Sprintf("%s.%s", svc.Name, svc.Namespace),
						Port:   int(svc.Spec.Ports[0].Port),
						Weight: 100,
					},
				},
			},
			Version: upstreamVersion,
		},
	}

	_, err = r.sdk.UpdateApplication(ctx, app, &cloud.ResourceUpdateOptions{
		ControlPlane: &cloud.ControlPlane{
			ID: r.controlPlane.ID,
		},
	})
	if err != nil {
		return errors.Wrap(err, "failed to update application (add upstream)")
	}

	rule := &cloud.CanaryRelease{
		CanaryReleaseSpec: cloud.CanaryReleaseSpec{
			Name:                  ruleName,
			State:                 cloud.CanaryReleaseStatePaused,
			Type:                  cloud.CanaryReleaseTypePercent,
			CanaryUpstreamVersion: upstreamVersion,
			Percent:               percentage,
		},
	}
	rule, err = r.sdk.CreateCanaryRelease(ctx, rule, &cloud.ResourceCreateOptions{
		Application: &cloud.Application{
			ID: app.ID,
		},
	})
	if err != nil {
		return errors.Wrap(err, "failed to create canary release rule")
	}

	if percentage == 100 {
		_, err = r.sdk.FinishCanaryRelease(ctx, rule, &cloud.ResourceUpdateOptions{
			Application: &cloud.Application{
				ID: app.ID,
			},
		})
		if err != nil {
			return errors.Wrap(err, "failed to finish canary release rule")
		}
	} else {
		_, err = r.sdk.StartCanaryRelease(ctx, rule, &cloud.ResourceUpdateOptions{
			Application: &cloud.Application{
				ID: app.ID,
			},
		})
		if err != nil {
			return errors.Wrap(err, "failed to start canary release rule")
		}
	}
	return nil
}
