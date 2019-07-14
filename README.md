# k8s-nlb-registrator-sidecar

Register EKS k8s pods as AWS NLB Targets in an IP based Target Group<Paste>

# Requirements
* Cluster that is utilising AWS VPC CNI for networking
* AWS Credentials to register and deregister targets in an ELBV2 LoadBalancer
* ELBv2 LoadBalancer

# Usage

```
./k8s-nlb-registrator-sidecar -h
Usage of ./k8s-nlb-registrator-sidecar:
  -post-deregister-command value
        Command to execute after target is deregistered
  -post-deregister-timeout value
        How long to wait for post-deregister command to execute (default 5s)
  -pre-register-command value
        Command to execute befre target is registered
  -pre-register-timeout value
        How long to wait for pre-register command to execute (default 5s)
  -target-group-name value
        Which target group to use for registering and deregistering targets
  -target-id value
        Target ID to use
  -wait-in-service
        Whether to wait for target group to become healthy (default true)
  -wait-in-service-timeout value
        How long to wait for target group to become healthy (default 5m0s)
```

## Disclaimer
* This is not the only way to do this - you can use bash scripts as `preStop` and `postStart` Kubernetes lifecycle hooks
* If you don't care for container native load-balancing you can achieve similar results with the Kubernetes Service Object.

## Motivation
• Enable IAC automation of NLBs used with Kubernetes - It turns out that it is really hard to automate creation of VPC PrivateLink connection if the NLB was created using Kubernetes Service Object
• Get rid of the bash scripts used in `preStop` and `postStart` Kubernetes lifecycle hooks, because ... bash
• Achieve something like https://cloud.google.com/blog/products/containers-kubernetes/introducing-container-native-load-balancing-on-google-kubernetes-engine for TCP services in Amazon

## Use cases
• Register TCP services in NLB with Target Type IP (Example: Running centralised Kafka broker and exposing it in multiple accounts using AWS PrivateLink - https://www.slideshare.net/ConfluentInc/connecting-kafka-across-multiple-aws-vpcs)
• Register your Edge proxy directly in NLB - useful for regulated environments where in-transit data encryption is mandatory

## How it works
1. When a pod starts the program will:
 * Gather it's IP from Kubernetes Downward API
 * Discover target group arn by performing the `elbv2:DescribeTargetGroups` with a filter - target group name (Downward API can be used here as well)
2.  Perform `elbv2:RegisterTargets` action to a named target group using the ip as TargetID.
3. (Optional) Wait for the target to become healthy by invoking the `WaitUntilTargetInServiceWithContext` Go SDK method (under the hood it polls `elbv2:DescribeTargetHealth`
4. Block and wait for process signal - SIGINT or  SIGTERM
5. When any of the signals described above is received the program will perform `elbv2:DeregisterTargets` action and cancel running `elbv2:RegisterTargets` if any.

When Kubernetes decides to delete the pod for some reason (rolling update, node draining, manual eviction, rebalancing) it will send SIGTERM signal to the sidecar container and the pod will be deregistered (draining) in the NLB Target Group.
Optionally, invoke command after `elbv2:DeregisterTargets` to notify other Container in the Pod that it is safe to stop receiving traffic.

## Current functionality
• Registering a pod in Target Group with type IP
• Wait until a target is Healthy in Target Group
• Deregister a target in Target Group
• Invoke command before registration and after deregistration

## TODO
• Docs
• Some Integration Tests
• CI Pipeline
• Refactor to follow design principles and patterns
