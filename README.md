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
