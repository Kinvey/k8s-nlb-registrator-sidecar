package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"github.com/go-kit/kit/log"
)

type RegisterTargetInput struct {
	ID                        *string
	TargetGroupArn            *string
	WaitUntilInService        *bool
	WaitUntilInServiceTimeout time.Duration
}

type DeregisterTargetInput struct {
	ID             *string
	TargetGroupArn *string
}

type Registrator interface {
	RegisterTarget(ctx context.Context, t *RegisterTargetInput) error
	DeregisterTarget(ctx context.Context, t *DeregisterTargetInput) error
}

type RegistratorService struct {
	ELBClient elbv2iface.ELBV2API
	Logger    log.Logger
}

func NewTargets(targetID *string) []*elbv2.TargetDescription {
	return []*elbv2.TargetDescription{
		&elbv2.TargetDescription{
			Id: targetID,
		},
	}
}

func (r *RegistratorService) RegisterTarget(ctx context.Context, t *RegisterTargetInput) error {

	if t.TargetGroupArn == nil {
		return errors.New("RegistratorService.TargetGroupArn is empty, please call DiscoverTargetGroupArn or set it with flag")
	}

	r.Logger.Log("msg", "Registering target in target group")
	targets := NewTargets(t.ID)
	_, err := r.ELBClient.RegisterTargetsWithContext(ctx, &elbv2.RegisterTargetsInput{
		Targets:        targets,
		TargetGroupArn: t.TargetGroupArn,
	})

	if err != nil {
		return err
	}

	if aws.BoolValue(t.WaitUntilInService) {
		ctx, cancel := context.WithTimeout(ctx, t.WaitUntilInServiceTimeout)
		defer cancel()

		r.Logger.Log("msg", "Waiting for target to be in service in target group")
		err = r.ELBClient.WaitUntilTargetInServiceWithContext(ctx, &elbv2.DescribeTargetHealthInput{
			TargetGroupArn: t.TargetGroupArn,
			Targets:        targets,
		})
		if err != nil {
			return err
		}
	}

	r.Logger.Log("msg", "Target is registered in target group")

	return nil
}

func (r *RegistratorService) DeregisterTarget(ctx context.Context, t *DeregisterTargetInput) error {
	r.Logger.Log("msg", "Deregistering target from target group")
	_, err := r.ELBClient.DeregisterTargetsWithContext(ctx, &elbv2.DeregisterTargetsInput{
		TargetGroupArn: t.TargetGroupArn,
		Targets:        NewTargets(t.ID),
	})
	if err != nil {
		return err
	}
	r.Logger.Log("msg", "Target is marked as deregistered in target group")
	return nil
}

func (r *RegistratorService) DiscoverTargetGroupArn(targetGroupName string) (string, error) {
	targetGroups, err := r.ELBClient.DescribeTargetGroups(&elbv2.DescribeTargetGroupsInput{
		Names: []*string{
			aws.String(targetGroupName),
		},
	})

	if err != nil {
		return "", err
	}

	if len(targetGroups.TargetGroups) != 1 {
		return "", fmt.Errorf("Unexpected count of target groups %d", len(targetGroups.TargetGroups))
	}

	return aws.StringValue(targetGroups.TargetGroups[0].TargetGroupArn), nil
}

func New(elbClient elbv2iface.ELBV2API, logger log.Logger) *RegistratorService {
	return &RegistratorService{ELBClient: elbClient, Logger: logger}
}
