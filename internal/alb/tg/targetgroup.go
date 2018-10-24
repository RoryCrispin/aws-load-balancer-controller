package tg

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/alb/tags"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albelbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/annotations"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/backend"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/controller/store"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/k8s"
	util "github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/util/types"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/types"
)

// The port used when creating targetGroup serves as a default value for targets registered without port specified.
// there are cases that a single targetGroup contains different ports, e.g. backend service targets multiple deployments with targetPort
// as "http", but "http" points to 80 or 8080 in different deployment.
// So we used a dummy(but valid) port number when creating targetGroup, and register targets with port number explicitly.
// see https://docs.aws.amazon.com/sdk-for-go/api/service/elbv2/#CreateTargetGroupInput
const targetGroupDefaultPort = 1

// Controller manages a single targetGroup for specific ingress & ingressBackend.
type Controller interface {
	// Reconcile ensures an targetGroup exists for specified backend of ingress.
	Reconcile(ctx context.Context, ingress *extensions.Ingress, backend extensions.IngressBackend) (TargetGroup, error)
}

func NewController(elbv2 albelbv2.ELBV2API, store store.Storer, nameTagGen NameTagGenerator, tagsController tags.Controller, endpointResolver backend.EndpointResolver) Controller {
	attrsController := NewAttributesController(elbv2)
	targetsController := NewTargetsController(elbv2, endpointResolver)
	return &defaultController{
		elbv2:             elbv2,
		store:             store,
		nameTagGen:        nameTagGen,
		tagsController:    tagsController,
		attrsController:   attrsController,
		targetsController: targetsController,
	}
}

var _ Controller = (*defaultController)(nil)

type defaultController struct {
	elbv2      albelbv2.ELBV2API
	store      store.Storer
	nameTagGen NameTagGenerator

	tagsController    tags.Controller
	attrsController   AttributesController
	targetsController TargetsController
}

func (controller *defaultController) Reconcile(ctx context.Context, ingress *extensions.Ingress, backend extensions.IngressBackend) (TargetGroup, error) {
	serviceAnnos, err := controller.loadServiceAnnotations(ingress, backend.ServiceName)
	if err != nil {
		return TargetGroup{}, fmt.Errorf("failed to load serviceAnnotation due to %v", err)
	}
	protocol := aws.StringValue(serviceAnnos.TargetGroup.BackendProtocol)
	targetType := aws.StringValue(serviceAnnos.TargetGroup.TargetType)
	tgName := controller.nameTagGen.NameTG(ingress.Namespace, ingress.Name, backend.ServiceName, backend.ServicePort.String(), targetType, protocol)
	tgInstance, err := controller.findExistingTGInstance(tgName)
	if err != nil {
		return TargetGroup{}, fmt.Errorf("failed to find existing targetGroup due to %v", err)
	}
	if tgInstance == nil {
		if tgInstance, err = controller.newTGInstance(ctx, tgName, serviceAnnos); err != nil {
			return TargetGroup{}, fmt.Errorf("failed to create targetGroup due to %v", err)
		}
	} else {
		if tgInstance, err = controller.reconcileTGInstance(ctx, tgInstance, serviceAnnos); err != nil {
			return TargetGroup{}, fmt.Errorf("failed to modify targetGroup due to %v", err)
		}
	}

	tgArn := aws.StringValue(tgInstance.TargetGroupArn)
	tgTags := controller.buildTags(ingress, backend)
	if err := controller.tagsController.Reconcile(ctx, &tags.Tags{Arn: tgArn, Tags: tgTags}); err != nil {
		return TargetGroup{}, fmt.Errorf("failed to reconcile targetGroup tags due to %v", err)
	}
	if err := controller.attrsController.Reconcile(ctx, tgArn, serviceAnnos.TargetGroup.Attributes); err != nil {
		return TargetGroup{}, fmt.Errorf("failed to reconcile targetGroup attributes due to %v", err)
	}
	tgTargets := NewTargets(targetType, ingress, &backend)
	tgTargets.TgArn = tgArn
	if err = controller.targetsController.Reconcile(ctx, tgTargets); err != nil {
		return TargetGroup{}, fmt.Errorf("failed to reconcile targetGroup targets due to %v", err)
	}
	return TargetGroup{
		Arn:        tgArn,
		TargetType: targetType,
		Targets:    tgTargets.Targets,
	}, nil
}

func (controller *defaultController) newTGInstance(ctx context.Context, name string, serviceAnnos *annotations.Service) (*elbv2.TargetGroup, error) {
	vpcID := controller.store.GetConfig().VpcID
	resp, err := controller.elbv2.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:                       aws.String(name),
		HealthCheckPath:            serviceAnnos.HealthCheck.Path,
		HealthCheckIntervalSeconds: serviceAnnos.HealthCheck.IntervalSeconds,
		HealthCheckPort:            serviceAnnos.HealthCheck.Port,
		HealthCheckProtocol:        serviceAnnos.HealthCheck.Protocol,
		HealthCheckTimeoutSeconds:  serviceAnnos.HealthCheck.TimeoutSeconds,
		TargetType:                 serviceAnnos.TargetGroup.TargetType,
		Protocol:                   serviceAnnos.TargetGroup.BackendProtocol,
		Matcher:                    &elbv2.Matcher{HttpCode: serviceAnnos.TargetGroup.SuccessCodes},
		HealthyThresholdCount:      serviceAnnos.TargetGroup.HealthyThresholdCount,
		UnhealthyThresholdCount:    serviceAnnos.TargetGroup.UnhealthyThresholdCount,
		Port:                       aws.Int64(targetGroupDefaultPort),
		VpcId:                      aws.String(vpcID),
	})
	if err != nil {
		return nil, err
	}
	return resp.TargetGroups[0], nil
}

func (controller *defaultController) reconcileTGInstance(ctx context.Context, instance *elbv2.TargetGroup, serviceAnnos *annotations.Service) (*elbv2.TargetGroup, error) {
	if controller.TGInstanceNeedsModification(ctx, instance, serviceAnnos) {
		if output, err := controller.elbv2.ModifyTargetGroup(&elbv2.ModifyTargetGroupInput{
			TargetGroupArn:             instance.TargetGroupArn,
			HealthCheckPath:            serviceAnnos.HealthCheck.Path,
			HealthCheckIntervalSeconds: serviceAnnos.HealthCheck.IntervalSeconds,
			HealthCheckPort:            serviceAnnos.HealthCheck.Port,
			HealthCheckProtocol:        serviceAnnos.HealthCheck.Protocol,
			HealthCheckTimeoutSeconds:  serviceAnnos.HealthCheck.TimeoutSeconds,
			Matcher:                    &elbv2.Matcher{HttpCode: serviceAnnos.TargetGroup.SuccessCodes},
			HealthyThresholdCount:      serviceAnnos.TargetGroup.HealthyThresholdCount,
			UnhealthyThresholdCount:    serviceAnnos.TargetGroup.UnhealthyThresholdCount,
		}); err != nil {
			return instance, err
		} else {
			return output.TargetGroups[0], err
		}
	}
	return instance, nil
}

func (controller *defaultController) TGInstanceNeedsModification(ctx context.Context, instance *elbv2.TargetGroup, serviceAnnos *annotations.Service) bool {
	needsChange := false
	if !util.DeepEqual(instance.HealthCheckPath, serviceAnnos.HealthCheck.Path) {
		needsChange = true
	}
	if !util.DeepEqual(instance.HealthCheckPort, serviceAnnos.HealthCheck.Port) {
		needsChange = true
	}
	if !util.DeepEqual(instance.HealthCheckProtocol, serviceAnnos.HealthCheck.Protocol) {
		needsChange = true
	}
	if !util.DeepEqual(instance.HealthCheckIntervalSeconds, serviceAnnos.HealthCheck.IntervalSeconds) {
		needsChange = true
	}
	if !util.DeepEqual(instance.HealthCheckTimeoutSeconds, serviceAnnos.HealthCheck.TimeoutSeconds) {
		needsChange = true
	}
	if !util.DeepEqual(instance.Matcher.HttpCode, serviceAnnos.TargetGroup.SuccessCodes) {
		needsChange = true
	}
	if !util.DeepEqual(instance.HealthyThresholdCount, serviceAnnos.TargetGroup.HealthyThresholdCount) {
		needsChange = true
	}
	if !util.DeepEqual(instance.UnhealthyThresholdCount, serviceAnnos.TargetGroup.UnhealthyThresholdCount) {
		needsChange = true
	}
	return needsChange
}

func (controller *defaultController) buildTags(ingress *extensions.Ingress, backend extensions.IngressBackend) map[string]string {
	tgTags := make(map[string]string)
	for k, v := range controller.nameTagGen.TagTGGroup(ingress.Namespace, ingress.Name) {
		tgTags[k] = v
	}
	for k, v := range controller.nameTagGen.TagTG(backend.ServiceName, backend.ServicePort.String()) {
		tgTags[k] = v
	}
	return tgTags
}

func (controller *defaultController) loadServiceAnnotations(ingress *extensions.Ingress, serviceName string) (*annotations.Service, error) {
	serviceKey := types.NamespacedName{Namespace: ingress.Namespace, Name: serviceName}
	ingressAnnos, err := controller.store.GetIngressAnnotations(k8s.MetaNamespaceKey(ingress))
	if err != nil {
		return nil, err
	}
	serviceAnnos, err := controller.store.GetServiceAnnotations(serviceKey.String(), ingressAnnos)
	if err != nil {
		return nil, err
	}
	return serviceAnnos, err
}

func (controller *defaultController) findExistingTGInstance(tgName string) (*elbv2.TargetGroup, error) {
	return controller.elbv2.GetTargetGroupByName(tgName)
}
