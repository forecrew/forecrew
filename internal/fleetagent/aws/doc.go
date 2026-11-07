// Package aws is the capacity provider of a customer's AWS account: each machine
// is one EC2 instance in the customer's VPC, started by a fleet agent that runs
// in that account with an instance role. The credentials are the role's and never
// leave the account; forecrewd holds none (docs/fleet.md, AWS locations).
//
// Everything the provider creates carries the tag forecrew:fleet, and the role
// the kit makes (deploy/byoc/aws) can only create, tag and terminate resources
// that carry it, so the agent touches machines it created and nothing else.
package aws
