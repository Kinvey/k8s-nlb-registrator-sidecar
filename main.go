package main

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/eapache/go-resiliency/retrier"
	"github.com/go-kit/kit/log"
	"sigs.k8s.io/controller-runtime/pkg/runtime/signals"
)

var (
	waitInServiceDurationTimeout, preRegisterCommandTimeout, postDeregisterCommandTimeout time.Duration
	targetID, targetGroupName, preRegisterCommand, postDeregisterCommand                  string
	waitInService, useKube2iam                                                            bool
)

func init() {
	flag.BoolVar(&waitInService, "wait-in-service", true, "Whether to wait for NLB target to become healthy")
	flag.DurationVar(&waitInServiceDurationTimeout, "wait-in-service-timeout", 5*time.Minute, "How long to wait for NLB target to become healthy")
	flag.StringVar(&targetID, "target-id", "", "TargetID to register in NLB")
	flag.StringVar(&targetGroupName, "target-group-name", "", "Target group name to look for")
	flag.StringVar(&postDeregisterCommand, "post-deregister-command", "", "Command to execute after target is deregistered from target group")
	flag.DurationVar(&postDeregisterCommandTimeout, "post-deregister-command-timeout", 5*time.Second, "How long to wait for pre-deregister-command to finish")
	flag.StringVar(&preRegisterCommand, "pre-register-command", "", "Command to execute befre target is registered with target group")
	flag.DurationVar(&preRegisterCommandTimeout, "pre-register-command-timeout", 5*time.Second, "How long to wait for pre-register-command to finish")
	flag.BoolVar(&useKube2iam, "kube2iam", false, "Whether pod uses kube2iam")
}

func main() {
	// Setup flags
	flag.Parse()

	var logger log.Logger
	logger = log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	logger = log.With(logger, "time", log.DefaultTimestampUTC, "caller", log.DefaultCaller)

	if useKube2iam {
		logger.Log("msg", "Give some time for kube2iam to setup temporary security credentials. (Sleeping for 10 seconds)")
		time.Sleep(time.Second * 10)
	}

	ctx := context.Background()
	// Setup graceful shutdown
	done := signals.SetupSignalHandler()

	var sess *session.Session

	// Setup dependencies
	sessionRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
	err := sessionRetrier.Run(func() error {
		var err error
		sess, err = session.NewSession()
		return err
	})

	if err != nil {
		logger.Log("error", err)
	}

	svc := elbv2.New(sess)
	registratorService := New(svc, logger)

	var targetGroupArn string
	discoverTargetGroupArnRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
	err = discoverTargetGroupArnRetrier.Run(func() error {
		var err error
		targetGroupArn, err = registratorService.DiscoverTargetGroupArn(targetGroupName)
		return err
	})

	if err != nil {
		logger.Log("error", err)
		os.Exit(1)
	}

	regCancelCtx, regCancelFunc := context.WithCancel(ctx)

	go func() {
		if preRegisterCommand != "" {
			logger.Log("msg", "Executing pre-register command", "command", preRegisterCommand)
			ExecCommand(ctx, logger, preRegisterCommand)
		}

		registerRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
		err = registerRetrier.RunCtx(regCancelCtx, func(ctx context.Context) error {
			return registratorService.RegisterTarget(ctx, &RegisterTargetInput{
				ID:                        aws.String(targetID),
				TargetGroupArn:            aws.String(targetGroupArn),
				WaitUntilInService:        aws.Bool(waitInService),
				WaitUntilInServiceTimeout: waitInServiceDurationTimeout,
			})
		})

		if err != nil {
			logger.Log("error", err)
		}
	}()

	logger.Log("msg", "Awaiting signal for deregistration")

	// Block and wait for signal
	<-done
	regCancelFunc()

	deregisterRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
	err = deregisterRetrier.RunCtx(ctx, func(context.Context) error {
		return registratorService.DeregisterTarget(ctx, &DeregisterTargetInput{
			ID:             aws.String(targetID),
			TargetGroupArn: aws.String(targetGroupArn),
		})
	})

	if err != nil {
		logger.Log("error", err)
	}

	ctx, cancel := context.WithTimeout(ctx, postDeregisterCommandTimeout)
	defer cancel()

	if postDeregisterCommand != "" {
		logger.Log("msg", "Executing post-deregister command", "command", postDeregisterCommand)
		ExecCommand(ctx, logger, postDeregisterCommand)
	}
}
