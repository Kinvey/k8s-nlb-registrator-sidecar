package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"k8s.io/klog"
)

func isEmptyString(s string) bool {
	return s == ""
}

var (
	waitInServiceDurationTimeout, preRegisterCommandTimeout, postDeregisterCommandTimeout time.Duration
	targetID, targetGroupName, preRegisterCommand, postDeregisterCommand                  string
	useKube2iam                                                                           bool
)

func init() {
	flag.DurationVar(&waitInServiceDurationTimeout, "wait-in-service-timeout", 5*time.Minute, "How long to wait for NLB target to become healthy")
	flag.StringVar(&targetID, "target-id", "", "TargetID to register in NLB")
	flag.StringVar(&targetGroupName, "target-group-name", "", "Target group name to look for")
	flag.StringVar(&postDeregisterCommand, "post-deregister-command", "", "Command to execute after target is deregistered from target group")
	flag.DurationVar(&postDeregisterCommandTimeout, "post-deregister-command-timeout", 5*time.Second, "How long to wait for pre-deregister-command to finish")
	flag.StringVar(&preRegisterCommand, "pre-register-command", "", "Command to execute befre target is registered with target group")
	flag.DurationVar(&preRegisterCommandTimeout, "pre-register-command-timeout", 5*time.Second, "How long to wait for pre-register-command to finish")
	flag.BoolVar(&useKube2iam, "kube2iam", false, "Whether pod uses kube2iam")
}

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

	klog.Infof("Registering %s as target to %#v", aws.StringValue(t.ID), aws.StringValue(t.TargetGroupArn))
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

		klog.Infof("Waiting for %s to be in service in target group", aws.StringValue(t.ID))
		err = r.ELBClient.WaitUntilTargetInServiceWithContext(ctx, &elbv2.DescribeTargetHealthInput{
			TargetGroupArn: t.TargetGroupArn,
			Targets:        targets,
		})
		if err != nil {
			return err
		}
	}

	klog.Infof("Target %#v is registered in target group %#v", aws.StringValue(t.ID), aws.StringValue(t.TargetGroupArn))

	return nil
}

func (r *RegistratorService) DeregisterTarget(ctx context.Context, t *DeregisterTargetInput) error {
	klog.Infof("Deregistering %s from %#v", aws.StringValue(t.ID), aws.StringValue(t.TargetGroupArn))
	_, err := r.ELBClient.DeregisterTargetsWithContext(ctx, &elbv2.DeregisterTargetsInput{
		TargetGroupArn: t.TargetGroupArn,
		Targets:        NewTargets(t.ID),
	})
	if err != nil {
		return err
	}
	klog.Infof("Target %s is marked as deregistered in target group", aws.StringValue(t.ID))
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

func ExecCommand(ctx context.Context, command string) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	combinedOutput, err := cmd.CombinedOutput()
	klog.Infoln(string(combinedOutput))
	return err
}

func New(elbClient elbv2iface.ELBV2API) *RegistratorService {
	return &RegistratorService{ELBClient: elbClient}
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	done := make(chan bool, 1)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		klog.Infof("Received %s", sig)
		done <- true
	}()

	if useKube2iam {
		klog.Infoln("Give some time for kube2iam to setup temporary security credentials. (Sleeping for 10 seconds)")
		time.Sleep(time.Second * 10)
	}

	sess := session.Must(session.NewSession())
	var elbClient *elbv2.ELBV2
	elbClient = elbv2.New(sess)

	registratorService := New(elbClient)

	ctx := context.Background()

	regCancelCtx, cancel := context.WithCancel(ctx)

	targetGroupArn, err := registratorService.DiscoverTargetGroupArn(targetGroupName)
	if err != nil {
		klog.Fatalln(err)
	}

	if preRegisterCommand != "" {
		klog.Infof("Executing pre-register command: %s", preRegisterCommand)
		ExecCommand(ctx, preRegisterCommand)
	}

	err = registratorService.RegisterTarget(regCancelCtx, &RegisterTargetInput{
		ID:                        aws.String(targetID),
		TargetGroupArn:            aws.String(targetGroupArn),
		WaitUntilInService:        aws.Bool(true),
		WaitUntilInServiceTimeout: waitInServiceDurationTimeout,
	})

	if err != nil {
		klog.Fatalln(err)
	}

	klog.Infoln("Awaiting signal for deregistration")

	// Block and wait for signal
	<-done

	err = registratorService.DeregisterTarget(ctx, &DeregisterTargetInput{
		ID:             aws.String(targetID),
		TargetGroupArn: aws.String(targetGroupArn),
	})

	if err != nil {
		klog.Fatalln(err)
	}

	ctx, cancel = context.WithTimeout(ctx, postDeregisterCommandTimeout)
	defer cancel()

	if postDeregisterCommand != "" {
		klog.Infof("Executing post-deregister command: %s", postDeregisterCommand)
		ExecCommand(ctx, postDeregisterCommand)
	}
}
