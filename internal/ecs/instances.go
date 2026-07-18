package ecs

// Instance → GameServer resolution for EC2 Spot interruption warnings and
// planned node drains. Fargate has no instances; this is
// EC2 capacity only.

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
)

// InstanceAPI is the ECS surface instance resolution uses.
type InstanceAPI interface {
	ListContainerInstances(ctx context.Context, in *awsecs.ListContainerInstancesInput, opts ...func(*awsecs.Options)) (*awsecs.ListContainerInstancesOutput, error)
	ListTasks(ctx context.Context, in *awsecs.ListTasksInput, opts ...func(*awsecs.Options)) (*awsecs.ListTasksOutput, error)
	DescribeTasks(ctx context.Context, in *awsecs.DescribeTasksInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeTasksOutput, error)
}

// InstanceResolver finds the arena GameServers running on an EC2 container
// instance via the startedBy linkage ("arena:{gameserver_id}").
type InstanceResolver struct {
	api     InstanceAPI
	cluster string
}

// NewInstanceResolver returns a resolver for the cluster.
func NewInstanceResolver(api InstanceAPI, cluster string) *InstanceResolver {
	return &InstanceResolver{api: api, cluster: cluster}
}

// GameServersOnInstance returns the gameserver IDs whose tasks run on the
// EC2 instance. An instance unknown to the cluster returns an empty list.
func (r *InstanceResolver) GameServersOnInstance(ctx context.Context, instanceID string) ([]string, error) {
	ci, err := r.api.ListContainerInstances(ctx, &awsecs.ListContainerInstancesInput{
		Cluster: aws.String(r.cluster),
		Filter:  aws.String(fmt.Sprintf("ec2InstanceId == '%s'", instanceID)),
	})
	if err != nil {
		return nil, fmt.Errorf("list container instances: %w", err)
	}
	if len(ci.ContainerInstanceArns) == 0 {
		return nil, nil
	}

	var gsIDs []string
	for _, ciARN := range ci.ContainerInstanceArns {
		var nextToken *string
		for {
			lt, err := r.api.ListTasks(ctx, &awsecs.ListTasksInput{
				Cluster:           aws.String(r.cluster),
				ContainerInstance: aws.String(ciARN),
				NextToken:         nextToken,
			})
			if err != nil {
				return nil, fmt.Errorf("list tasks on %s: %w", ciARN, err)
			}
			if len(lt.TaskArns) > 0 {
				dt, err := r.api.DescribeTasks(ctx, &awsecs.DescribeTasksInput{
					Cluster: aws.String(r.cluster),
					Tasks:   lt.TaskArns,
				})
				if err != nil {
					return nil, fmt.Errorf("describe tasks: %w", err)
				}
				for _, t := range dt.Tasks {
					if id, ok := ParseStartedBy(aws.ToString(t.StartedBy)); ok {
						gsIDs = append(gsIDs, id)
					}
				}
			}
			if nextToken = lt.NextToken; nextToken == nil {
				break
			}
		}
	}
	return gsIDs, nil
}
