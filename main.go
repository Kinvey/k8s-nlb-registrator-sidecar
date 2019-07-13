package main

import (
	"context"
	"errors"
	"flag"
	"log"
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
	waitInServiceDurationTimeout, preStopTimeout     time.Duration
	targetID, targetGroupName, postDeregisterCommand string
	useKube2iam                                      bool
)

func init() {
	flag.DurationVar(&waitInServiceDurationTimeout, "wait-in-service-timeout", 5*time.Minute, "How long to wait for NLB target to become healthy")
	flag.StringVar(&targetID, "target-id", "", "TargetID to register in NLB")
	flag.StringVar(&targetGroupName, "target-group-name", "", "Target group name to look for")
	flag.StringVar(&postDeregisterCommand, "post-deregister-command", "", "Command to execute after target is deregistered from target group")
	flag.DurationVar(&preStopTimeout, "post-deregister-command-timeout", 5*time.Second, "How long to wait for pre-deregister-command to finish")
	flag.BoolVar(&useKube2iam, "kube2iam", false, "Whether pod uses kube2iam")
}

type RegisterTargetInput struct {
	ID                        *string
	TargetGroupArn            *string
	WaitUntilInService        *bool
	WaitUntilInServiceTimeout time.Duration
}

type Registrator interface {
	RegisterTarget(ctx context.Context, t *RegisterTargetInput) error
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

func New(elbClient elbv2iface.ELBV2API) *RegistratorService {
	return &RegistratorService{ELBClient: elbClient}
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	sigs := make(chan os.Signal, 1)
	done := make(chan bool, 1)

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

	targetGroups, err := elbClient.DescribeTargetGroups(&elbv2.DescribeTargetGroupsInput{
		Names: []*string{
			aws.String(targetGroupName),
		},
	})

	if err != nil {
		klog.Fatalln(err)
	}

	// We expect single target group
	targetGroupArn := targetGroups.TargetGroups[0].TargetGroupArn

	targetDescription := []*elbv2.TargetDescription{
		&elbv2.TargetDescription{
			Id: aws.String(targetID),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	err = registratorService.RegisterTarget(ctx, &RegisterTargetInput{
		ID:                        aws.String(targetID),
		TargetGroupArn:            targetGroupArn,
		WaitUntilInService:        aws.Bool(true),
		WaitUntilInServiceTimeout: waitInServiceDurationTimeout,
	})
	// _, err = elbClient.RegisterTargets(&elbv2.RegisterTargetsInput{
	// 	TargetGroupArn: targetGroupArn,
	// 	Targets:        targetDescription,
	// })

	if err != nil {
		klog.Fatalln(err)
	}

	// describeTargetHealthInput := &elbv2.DescribeTargetHealthInput{
	// 	TargetGroupArn: targetGroupArn,
	// 	Targets:        targetDescription,
	// }

	// waitRegisterCtx, cancel := context.WithTimeout(context.Background(), waitInServiceDurationTimeout)
	// defer cancel()
	// err = elbClient.WaitUntilTargetInServiceWithContext(waitRegisterCtx, describeTargetHealthInput)

	// if err != nil {
	// 	log.Fatalln(err)
	// }

	// klog.Infof("Target %s is in service in target group", targetID)

	klog.Infoln("Awaiting signal for deregistration")

	// Block and wait for signal
	<-done

	// Kubernetes sent signal, deregister target from Load Balancer
	klog.Infof("Deregistering %s from %#v", targetID, aws.StringValue(targetGroupArn))
	_, err = elbClient.DeregisterTargets(&elbv2.DeregisterTargetsInput{
		TargetGroupArn: targetGroupArn,
		Targets:        targetDescription,
	})

	if err != nil {
		klog.Fatalln(err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), preStopTimeout)
	defer cancel()

	if postDeregisterCommand != "" {
		klog.Infof("Executing post-deregister command: %s", postDeregisterCommand)
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", postDeregisterCommand)
		combinedOutput, err := cmd.CombinedOutput()
		klog.Infoln(string(combinedOutput))
		if err != nil {
			log.Fatalln(err)
		}
	}

	klog.Infof("Target %s is marked as deregistered in target group", targetID)

	// I don't know if we should wait for the target to be deregistered to continue ?
	// Maybe not
	// waitDeregisterCtx, cancel := context.WithTimeout(context.Background(), time.Minute*10)
	// defer cancel()
	// log.Printf("Waiting for %s to be deregistered from target group", targetID)
	// err = elbClient.WaitUntilTargetDeregisteredWithContext(waitDeregisterCtx, describeTargetHealthInput)

	// if err != nil {
	// 	log.Fatalln(err)
	// }
}
