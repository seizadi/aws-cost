package svc

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/spf13/viper"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	ceTypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/golang/protobuf/ptypes/empty"

	"github.com/seizadi/cost-insights-backend/metrics"
	"github.com/seizadi/cost-insights-backend/pkg/pb"
	"github.com/seizadi/cost-insights-backend/pkg/types"
	"github.com/seizadi/cost-insights-backend/pkg/utils"
)

type costInsightsAwsServer struct {
	client *costexplorer.Client
}

var AWS_SERVICE = map[string]string{
	"EC2":           "Amazon Elastic Compute Cloud - Compute",
	"EC2Other":      "EC2 - Other",
	"S3":            "Amazon Simple Storage Service",
	"DynamoDB":      "Amazon DynamoDB",
	"ElasticSearch": "Amazon Elasticsearch Service",
	"CloudWatch":    "Amazon CloudWatch",
	"CloudTrail":    "Amazon CloudTrail",
	"RDS":           "Amazon Relational Database Service",
	"ELB":           "Amazon Elastic Load Balancing",
	"EMR":           "Amazon Elastic MadReduce",
	"MSK":           "Amazon Managed Streaming for Apache Kafka",
	"Lambda":        "AWS Lambda",
	"SNS":           "Amazon Simple Notification Service",
	"SQS":           "Amazon Simple Queue Service",
}

type AwsAccountType string

const (
	DeveloperAccount  AwsAccountType = "DEVELOPER"
	BusinessAccount   AwsAccountType = "BUSINESS"
	EnterpriseAccount AwsAccountType = "ENTERPRISE"
)

type AwsAccount struct {
	AccountType           AwsAccountType
	MinSupportCost        float64
	SupportCostThresholds []SupportCostThreshold
}

type SupportCostThreshold struct {
	CostMultiplier    float64
	CostStartInterval float64
	CostEndInterval   float64
}

var AwsAccounts = map[AwsAccountType]AwsAccount{
	DeveloperAccount: {
		DeveloperAccount,
		29.00,
		[]SupportCostThreshold{
			{0.03, 0, 0},
		},
	},
	BusinessAccount: {
		BusinessAccount,
		100.00,
		[]SupportCostThreshold{
			{0.10, 0, 10000.00},
			{0.07, 10000.00, 80000.00},
			{0.05, 80000.00, 250000.00},
			{0.03, 250000.00, 0},
		},
	},
	EnterpriseAccount: {
		EnterpriseAccount,
		15000.00,
		[]SupportCostThreshold{
			{0.10, 0, 150000.00},
			{0.07, 150000.00, 500000.00},
			{0.05, 500000.00, 1000000.00},
			{0.03, 1000000.00, 0},
		},
	},
}

func NewCeClient() (*costexplorer.Client, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, err
	}

	client := costexplorer.NewFromConfig(cfg)
	return client, nil
}

// NewCostInsightsApiAwsServer
// returns an instance of the default server interface
func NewCostInsightsApiAwsServer() (pb.CostInsightsApiServer, error) {
	client, err := NewCeClient()
	if err != nil {
		return nil, err
	}

	return &costInsightsAwsServer{client: client}, nil
}

// getAwsMetricAmount
// Retrieves the Cost Amount from AWS CostExplorer API
// TODO - We ignore Units asssume number USD (could support other units)
func getAwsMetricAmount(metric ceTypes.MetricValue) float64 {
	amount, _ := strconv.ParseFloat(*metric.Amount, 64)
	if viper.GetBool("cost.round") {
		return math.Round(amount)
	}
	return amount
}

// aggregationForAWS
// Transforms AWS CostExplorer ResultByTime array to CostInsights DateAggregation array
//
func aggregationForAWS(results []ceTypes.ResultByTime) ([]*pb.DateAggregation, error) {
	retDateAggregation := []*pb.DateAggregation{}
	supCost := 0.00

	if viper.GetBool("support.cost") {
		awsTest := AwsAccountType(viper.GetString("account.type"))
		supportCost, _ := SupportCostForAWS(AwsAccounts[awsTest], results)
		supCost = supportCost
	}

	for _, result := range results {
		value := pb.DateAggregation{
			Date: *result.TimePeriod.Start,
		}
		// We expect only one metric 'UnblendedCost' in the map but we could query more
		for _, metric := range result.Total {
			value.Amount = getAwsMetricAmount(metric) + supCost/float64(len(results))
		}

		if value.Amount > 0 {
			retDateAggregation = append(retDateAggregation, &value)
		}
	}

	//TODO enter the correct configuration for the account
	//TODO add the support Cost to the aggregation data somehow/split it across all the dates of aggregation?
	//	for _, date := range retDateAggregation {
	//		date.Amount = date.Amount+supportCost/float64(numDates)
	//	}

	return retDateAggregation, nil
}

func SupportCostForAWS(account AwsAccount, results []ceTypes.ResultByTime) (float64, error) {

	var sumDateAggregationAmounts float64
	var supportCost float64
	for _, result := range results {
		amount, _ := strconv.ParseFloat(*result.Total[string(ceTypes.MetricNetAmortizedCost)].Amount, 64)
		sumDateAggregationAmounts += amount
	}

	if (sumDateAggregationAmounts * account.SupportCostThresholds[0].CostMultiplier) < account.MinSupportCost {
		return account.MinSupportCost, nil
	}

	for _, costThreshold := range account.SupportCostThresholds {
		if costThreshold.CostStartInterval > sumDateAggregationAmounts {
			return supportCost, nil
		}
		if costThreshold.CostEndInterval != 0 {
			if costThreshold.CostEndInterval > sumDateAggregationAmounts {
				supportCost += (sumDateAggregationAmounts - costThreshold.CostStartInterval) * costThreshold.CostMultiplier
				return supportCost, nil
			}
			if sumDateAggregationAmounts > costThreshold.CostEndInterval {
				supportCost += (costThreshold.CostEndInterval - costThreshold.CostStartInterval) * costThreshold.CostMultiplier
			}
		} else {
			supportCost += (sumDateAggregationAmounts - costThreshold.CostStartInterval) * costThreshold.CostMultiplier
			return supportCost, nil
		}
	}

	return supportCost, nil
}

// getGroupedAwsKeyIndex
// Retrieve the AWS Service Names
// These Keys are not very predicatable then can be in the array in any order.
// they are verbose "AWS VPN (10 connected devices)" so if there are more connected devices in the same
// month maybe the string for AWS VPN might change? We have to parse and add all AWS VPN items?
// We build a map for all AWS Service name in the range so we can accomodate all the possible values.
// The map is used to lookup the index for building the grouping for CostInsights API.
//
func getGroupedAwsKeyIndex(results []ceTypes.ResultByTime) map[string]int {
	keys := make(map[string]int)

	for _, result := range results {
		for _, group := range result.Groups {
			if _, ok := keys[group.Keys[0]]; !ok {
				index := len(keys)
				keys[group.Keys[0]] = index
			}
		}
	}
	return keys
}

// getGroupedAwsProducts
// Retrieves Grouped AWS Products (i.e. AWS Services) Costs from CostExplorer API
//
func getGroupedAwsProducts(results []ceTypes.ResultByTime) ([]*pb.ProductCost, error) {
	keys := getGroupedAwsKeyIndex(results)
	costs := make([]*pb.ProductCost, len(keys))

	for key, index := range keys {
		productCost := &pb.ProductCost{
			Id:          key,
			Aggregation: []*pb.DateAggregation{},
		}
		costs[index] = productCost
	}

	for _, result := range results {
		for _, group := range result.Groups {
			cost := costs[keys[group.Keys[0]]]
			value := pb.DateAggregation{
				Date: *result.TimePeriod.Start,
			}
			// We expect only one metric 'UnblendedCost' in the map but we could query more
			for _, metric := range group.Metrics {
				value.Amount = getAwsMetricAmount(metric)
			}
			if value.Amount > 0 {
				cost.Aggregation = append(cost.Aggregation, &value)
			}
			costs[keys[group.Keys[0]]] = cost
		}
	}

	filteredCosts := []*pb.ProductCost{}
	for _, index := range keys {
		if len(costs[index].Aggregation) > 0 {
			filteredCosts = append(filteredCosts, costs[index])
		}
	}

	return filteredCosts, nil
}

// getGroupedAwsProjects
// Retrieves Grouped AWS Projects (i.e. AWS Accounts) Costs from CostExplorer API
//
func getGroupedAwsProjects(results []ceTypes.ResultByTime) ([]*pb.ProjectCost, error) {
	keys := getGroupedAwsKeyIndex(results)
	costs := make([]*pb.ProjectCost, len(keys))

	for key, index := range keys {
		projectCost := &pb.ProjectCost{
			Id:          key,
			Aggregation: []*pb.DateAggregation{},
		}
		costs[index] = projectCost
	}

	for _, result := range results {
		for _, group := range result.Groups {
			cost := costs[keys[group.Keys[0]]]
			value := pb.DateAggregation{
				Date: *result.TimePeriod.Start,
			}
			// We expect only one metric 'UnblendedCost' in the map but we could query more
			for _, metric := range group.Metrics {
				value.Amount = getAwsMetricAmount(metric)
			}
			if value.Amount > 0 {
				cost.Aggregation = append(cost.Aggregation, &value)
			}
			costs[keys[group.Keys[0]]] = cost
		}
	}

	filteredCosts := []*pb.ProjectCost{}
	for _, index := range keys {
		if len(costs[index].Aggregation) > 0 {
			filteredCosts = append(filteredCosts, costs[index])
		}
	}

	return filteredCosts, nil
}

// getEntityAwsProducts
// Retrieves Entities for AWS Products (i.e. AWS Services) Costs from CostExplorer API
//
func getEntityAwsProducts(results []ceTypes.ResultByTime) ([]*pb.Entity, error) {
	keys := getGroupedAwsKeyIndex(results)
	costs := make([]*pb.Entity, len(keys))

	for key, index := range keys {
		entity := &pb.Entity{
			Id: key,
			// TODO - Fix the hard coded values for Aggregation and Change
			Aggregation: []float64{0, 0},
			Change:      &pb.ChangeStatistic{},
			Entities:    &pb.Record{},
		}
		costs[index] = entity
	}

	// TODO - Compute Aggregation and Change
	// The ResultsByTime objects provide a Groups array with an entry for each resource and its costs for the
	// given day. You'll need to aggregate cost data into two bucketed time periods (e.g. month vs month,
	// or quarter vs quarter) for each resource since this is the expected data type for the Aggregation
	// field on Entity.

	midPoint := len(results) / 2

	for i, result := range results {
		for _, group := range result.Groups {
			var amount float64
			// We expect only one metric 'UnblendedCost' in the map but we could query more
			for _, metric := range group.Metrics {
				amount = getAwsMetricAmount(metric)
			}

			if i >= midPoint {
				costs[keys[group.Keys[0]]].Aggregation[1] = costs[keys[group.Keys[0]]].Aggregation[1] + amount
			} else {
				costs[keys[group.Keys[0]]].Aggregation[0] = costs[keys[group.Keys[0]]].Aggregation[0] + amount
			}
		}
	}

	filteredCosts := []*pb.Entity{}
	for _, index := range keys {
		if !(costs[index].Aggregation[0] == 0 && costs[index].Aggregation[1] == 0) {
			costs[index].Change = utils.ChangeOfEntity(costs[index].Aggregation)
			filteredCosts = append(filteredCosts, costs[index])
		}
	}

	return filteredCosts, nil
}

// GetLastCompleteBillingDate
// returns the most current date for which billing data is complete, in YYYY-MM-DD format. This helps
// define the intervals used in other API methods to avoid showing incomplete cost. The costs for
// today, for example, will not be complete. This ideally comes from the cloud provider.
//
// Implements CostInsightsApiClient getLastCompleteBillingDate(): Promise<string>;
func (costInsightsAwsServer) GetLastCompleteBillingDate(context.Context, *empty.Empty) (*pb.LastCompleteBillingDateResponse, error) {
	date := time.Now().AddDate(0, 0, -1).Format(types.DEFAULT_DATE_FORMAT)
	return &pb.LastCompleteBillingDateResponse{Date: date}, nil
}

// GetUserGroups
// Get a list of groups the given user belongs to. These may be LDAP groups or similar
// organizational groups. Cost Insights is designed to show costs based on group membership;
// if a user has multiple groups, they are able to switch between groups to see costs for each.
//
// This method should be removed once the Backstage identity plugin provides the same concept.
//
// @param userId The login id for the current user
//
// Implements CostInsightsApiClient getUserGroups(userId: string): Promise<Group[]>;
//

func (costInsightsAwsServer) GetUserGroups(context.Context, *pb.UserGroupsRequest) (*pb.UserGroupsResponse, error) {
	groups := []*pb.Group{
		{Id: "default-group"},
	}
	return &pb.UserGroupsResponse{Groups: groups}, nil
}

// GetUserGroups
// Get a list of cloud billing entities that belong to this group (projects in GCP, AWS has a
// similar concept in billing accounts). These act as filters for the displayed costs, users can
// choose whether they see all costs for a group, or those from a particular owned project.
//
// @param group The group id from getUserGroups or query parameters
// Implements CostInsightsApiClient getGroupProjects(group: string): Promise<Project[]>;
//TODO: Make a call to AWS and find all the accounts that a particular user has access to
func (costInsightsAwsServer) GetGroupProjects(context.Context, *pb.GroupProjectsRequest) (*pb.GroupProjectsResponse, error) {
	projects := []*pb.Project{
		{Id: "project-a"},
		{Id: "project-b"},
		{Id: "project-c"},
	}
	return &pb.GroupProjectsResponse{Projects: projects}, nil
}

// GetGroupDailyCost
// Get daily cost aggregations for a given group and interval time frame.
//
// The return type includes an array of daily cost aggregations as well as statistics about the
// change in cost over the intervals. Calculating these statistics requires us to bucket costs
// into two or more time periods, hence a repeating interval format rather than just a start and
// end date.
//
// The rate of change in this comparison allows teams to reason about their cost growth (or
// reduction) and compare it to metrics important to the business.
//
// @param group The group id from getUserGroups or query parameters
// @param intervals An ISO 8601 repeating interval string, such as R2/P30D/2020-09-01
//   https://en.wikipedia.org/wiki/ISO_8601#Repeating_intervals
//
// Implements CostInsightsApiClient getGroupDailyCost(group: string, intervals: string): Promise<Cost>;
func (m costInsightsAwsServer) GetGroupDailyCost(ctx context.Context, req *pb.GroupDailyCostRequest) (*pb.GroupDailyCostResponse, error) {
	cost := pb.GroupDailyCostResponse{}
	cost.Format = "number"

	interval, err := utils.ParseIntervals(req.Intervals)
	if err != nil {
		return nil, err
	}

	startDate, err := utils.InclusiveStartDateOf(interval.Duration, interval.EndDate)
	if err != nil {
		return nil, err
	}

	resp, err := m.client.GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
		TimePeriod: &ceTypes.DateInterval{Start: &startDate, End: &interval.EndDate},
		Metrics:    []string{viper.GetString("cost.aws.datasets")},
		// TODO - Need a way to map Group to Account(i.e. Project) to filter
		//Filter: &ceTypes.Expression{
		//	Dimensions: &ceTypes.DimensionValues{
		//		Key: ceTypes.DimensionLinkedAccount,
		//		Values: []string{"ACCOUNT_ID"},
		//	},
		//},
		Granularity: ceTypes.GranularityDaily,
	})
	if err != nil {
		return nil, err
	}

	aggregation, err := aggregationForAWS(resp.ResultsByTime)
	if err != nil {
		return &pb.GroupDailyCostResponse{}, err
	}
	cost.Aggregation = aggregation
	cost.Change = utils.ChangeOf(aggregation)
	trendline, err := utils.TrendlineOf(aggregation)
	if err != nil {
		return &pb.GroupDailyCostResponse{}, err
	}
	cost.Trendline = trendline

	// Optional field providing cost groupings / breakdowns keyed by the type. In this example,
	// daily cost grouped by cloud product OR by project / billing account.
	cost.GroupedCosts = &pb.GroupedCosts{}

	groupKey := "SERVICE"
	respProductGrouped, err := m.client.GetCostAndUsage(context.TODO(), &costexplorer.GetCostAndUsageInput{
		TimePeriod: &ceTypes.DateInterval{Start: &startDate, End: &interval.EndDate},
		Metrics:    []string{viper.GetString("cost.aws.datasets")},
		// TODO - Need a way to map Group to Account(i.e. Project) to filter
		//Filter: &ceTypes.Expression{
		//	Dimensions: &ceTypes.DimensionValues{
		//		Key: ceTypes.DimensionLinkedAccount,
		//		Values: []string{"ACCOUNT_ID"},
		//	},
		//},
		Granularity: ceTypes.GranularityDaily,
		GroupBy: []ceTypes.GroupDefinition{
			{Key: &groupKey, Type: ceTypes.GroupDefinitionTypeDimension},
		},
	})
	if err != nil {
		return nil, err
	}

	cost.GroupedCosts.Product, err = getGroupedAwsProducts(respProductGrouped.ResultsByTime)
	if err != nil {
		return &cost, err
	}

	// Optional field providing cost groupings / breakdowns keyed by the type. In this example,
	// daily cost grouped by cloud product OR by project / billing account.
	groupKey = "LINKED_ACCOUNT"
	respProjectGrouped, err := m.client.GetCostAndUsage(context.TODO(), &costexplorer.GetCostAndUsageInput{
		TimePeriod: &ceTypes.DateInterval{Start: &startDate, End: &interval.EndDate},
		Metrics:    []string{viper.GetString("cost.aws.datasets")},
		// TODO - Need a way to map Group to Account(i.e. Project) to filter
		//Filter: &ceTypes.Expression{
		//	Dimensions: &ceTypes.DimensionValues{
		//		Key: ceTypes.DimensionLinkedAccount,
		//		Values: []string{"ACCOUNT_ID"},
		//	},
		//},
		Granularity: ceTypes.GranularityDaily,
		GroupBy: []ceTypes.GroupDefinition{
			{Key: &groupKey, Type: ceTypes.GroupDefinitionTypeDimension},
		},
	})
	if err != nil {
		return nil, err
	}

	cost.GroupedCosts.Project, err = getGroupedAwsProjects(respProjectGrouped.ResultsByTime)
	if err != nil {
		return &cost, err
	}

	return &cost, nil
}

// GetDailyMetricData
//    * Get aggregations for a particular metric and interval time frame. Teams
//    * can see metrics important to their business in comparison to the growth
//    * (or reduction) of a project or group's daily costs.
//    *
//    * @param metric A metric from the cost-insights configuration in app-config.yaml.
//    * @param intervals An ISO 8601 repeating interval string, such as R2/P30D/2020-09-01
//    *   https://en.wikipedia.org/wiki/ISO_8601#Repeating_intervals
//    */
// Implements CostInsightsApiClient getDailyMetricData(metric: string, intervals: string): Promise<MetricData>;
func (costInsightsAwsServer) GetDailyMetricData(ctx context.Context, req *pb.DailyMetricDataRequest) (*pb.DailyMetricDataResponse, error) {
	cost := pb.DailyMetricDataResponse{}
	cost.Format = "number"
	aggregation, err := metrics.GetMetrics(req.Metric, req.Intervals)
	if err != nil {
		return &pb.DailyMetricDataResponse{}, err
	}

	//aggregation, err := utils.AggregationFor(req.Intervals, types.DAILY_COST)
	//if err != nil {
	//	return &pb.DailyMetricDataResponse{}, err
	//}
	cost.Aggregation = aggregation
	cost.Change = utils.ChangeOf(aggregation)
	trendline, err := utils.TrendlineOf(aggregation)
	if err != nil {
		return &pb.DailyMetricDataResponse{}, err
	}
	cost.Trendline = trendline

	return &cost, nil
}

// GetProjectDailyCost
// Get daily cost aggregations for a given billing entity (project in GCP, AWS has a similar
// concept in billing accounts) and interval time frame.
//
// The return type includes an array of daily cost aggregations as well as statistics about the
// change in cost over the intervals. Calculating these statistics requires us to bucket costs
// into two or more time periods, hence a repeating interval format rather than just a start and
// end date.
//
// The rate of change in this comparison allows teams to reason about the project's cost growth
// (or reduction) and compare it to metrics important to the business.
//
// @param project The project id from getGroupProjects or query parameters
// @param intervals An ISO 8601 repeating interval string, such as R2/P30D/2020-09-01
//   https://en.wikipedia.org/wiki/ISO_8601#Repeating_intervals
//
// Implements CostInsightsApiClient getProjectDailyCost(project: string, intervals: string): Promise<Cost>;
func (m costInsightsAwsServer) GetProjectDailyCost(ctx context.Context, req *pb.ProjectDailyCostRequest) (*pb.ProjectDailyCostResponse, error) {
	cost := pb.ProjectDailyCostResponse{}
	cost.Format = "number"
	interval, err := utils.ParseIntervals(req.Intervals)
	if err != nil {
		return nil, err
	}

	startDate, err := utils.InclusiveStartDateOf(interval.Duration, interval.EndDate)
	if err != nil {
		return nil, err
	}

	resp, err := m.client.GetCostAndUsage(context.TODO(), &costexplorer.GetCostAndUsageInput{
		TimePeriod: &ceTypes.DateInterval{Start: &startDate, End: &interval.EndDate},
		Metrics:    []string{viper.GetString("cost.aws.datasets")},
		// TODO - Need a way to map Project to Account to filter Project Detail
		//Filter: &ceTypes.Expression{
		//	Dimensions: &ceTypes.DimensionValues{
		//		Key: ceTypes.DimensionLinkedAccount,
		//		Values: []string{"ACCOUNT_ID"},
		//	},
		//},
		Granularity: ceTypes.GranularityDaily,
	})
	if err != nil {
		return nil, err
	}

	aggregation, err := aggregationForAWS(resp.ResultsByTime)
	if err != nil {
		return &pb.ProjectDailyCostResponse{}, err
	}
	cost.Aggregation = aggregation
	cost.Change = utils.ChangeOf(aggregation)
	trendline, err := utils.TrendlineOf(aggregation)
	if err != nil {
		return &pb.ProjectDailyCostResponse{}, err
	}
	cost.Trendline = trendline

	// Optional field providing cost groupings / breakdowns keyed by the type. In this example,
	// daily cost grouped by cloud product (AWS Service)
	cost.GroupedCosts = &pb.GroupedCosts{}

	groupKey := "SERVICE"
	respGrouped, err := m.client.GetCostAndUsage(context.TODO(), &costexplorer.GetCostAndUsageInput{
		TimePeriod: &ceTypes.DateInterval{Start: &startDate, End: &interval.EndDate},
		Metrics:    []string{viper.GetString("cost.aws.datasets")},
		// TODO - Need Account(i.e. Project) to filter
		//Filter: &ceTypes.Expression{
		//	Dimensions: &ceTypes.DimensionValues{
		//		Key: ceTypes.DimensionLinkedAccount,
		//		Values: []string{"ACCOUNT_ID"},
		//	},
		//},
		Granularity: ceTypes.GranularityDaily,
		GroupBy: []ceTypes.GroupDefinition{
			{Key: &groupKey, Type: ceTypes.GroupDefinitionTypeDimension},
		},
	})
	if err != nil {
		return nil, err
	}

	cost.GroupedCosts.Product, err = getGroupedAwsProducts(respGrouped.ResultsByTime)
	if err != nil {
		return &cost, err
	}

	return &cost, nil
}

// GetProductInsights
// Get cost aggregations for a particular cloud product and interval time frame. This includes
// total cost for the product, as well as a breakdown of particular entities that incurred cost
// in this product. The type of entity depends on the product - it may be deployed services,
// storage buckets, managed database instances, etc.
//
// If project is supplied, this should only return product costs for the given billing entity
// (project in GCP).
//
// The time period is supplied as a Duration rather than intervals, since this is always expected
// to return data for two bucketed time period (e.g. month vs month, or quarter vs quarter).
//
// @param Project to filter for only a specific Project
// @param Group to filter for query
// @param Product to filter only selected cloud product
// @param Interval to filter for the selected duration of time
//
// Implements CostInsightsApiClient getProductInsights(options: ProductInsightsOptions): Promise<Entity>;
func (m costInsightsAwsServer) GetProductInsights(ctx context.Context, req *pb.ProductInsightsRequest) (*pb.Entity, error) {
	// TODO - Need to be able to specify the cost Tag(s) that are used for the query
	// TODO - Need able to filter based on Product, Project or Group

	entity := &pb.Entity{}

	interval, err := utils.ParseIntervals(req.Intervals)
	if err != nil {
		return nil, err
	}

	startDate, err := utils.InclusiveStartDateOf(interval.Duration, interval.EndDate)
	if err != nil {
		return nil, err
	}

	// TODO - groupKey is the Cost Tag Name should be configurable (defaults to Product)
	groupKey := "Product"

	resp, err := m.client.GetCostAndUsage(context.TODO(), &costexplorer.GetCostAndUsageInput{
		TimePeriod: &ceTypes.DateInterval{Start: &startDate, End: &interval.EndDate},
		Metrics:    []string{viper.GetString("cost.aws.datasets")},
		// TODO - Need Account(i.e. Project) to filter
		// TODO - Use Group to select Account(s) (i.e. Projects) to filter
		Filter: &ceTypes.Expression{
			Dimensions: &ceTypes.DimensionValues{
				Key:    ceTypes.DimensionService,
				Values: []string{AWS_SERVICE[req.Product]},
			},
		},
		Granularity: ceTypes.GranularityDaily,
		GroupBy: []ceTypes.GroupDefinition{
			{Key: &groupKey, Type: ceTypes.GroupDefinitionTypeTag},
		},
	})
	if err != nil {
		return nil, err
	}

	entity.Id = req.Product

	entities, err := getEntityAwsProducts(resp.ResultsByTime)
	if err != nil {
		return entity, err
	}

	entity.Entities = &pb.Record{Service: entities}

	// We aggregate cost data into two bucketed time periods (e.g. month vs month, or quarter vs quarter).
	// For each half we will walk through the Entities and add their aggregate to form the Aggregation
	//field on Entity.

	var startAggregate float64
	var endAggregate float64
	for _, e := range entity.Entities.Service {
		startAggregate = startAggregate + e.Aggregation[0]
		endAggregate = endAggregate + e.Aggregation[1]
	}
	entity.Aggregation = []float64{startAggregate, endAggregate}
	entity.Change = utils.ChangeOfEntity(entity.Aggregation)

	return entity, nil
}

// GetAlerts
//
// Get current cost alerts for a given group. These show up as Action Items for the group on the
// Cost Insights page. Alerts may include cost-saving recommendations, such as infrastructure
// migrations, or cost-related warnings, such as an unexpected billing anomaly.
//
// Implements CostInsightsApiClient getAlerts(group: string): Promise<Alert[]>;
func (m costInsightsAwsServer) GetAlerts(ctx context.Context, req *pb.AlertRequest) (*pb.AlertResponse, error) {
	alerts := []*pb.Entity{}
	growthAlert, err := m.ProjectGrowthAlert()
	if err != nil {
		return &pb.AlertResponse{}, err
	}

	alerts = append(alerts, growthAlert)

	//unlabeledAlert, err := UnlabeledAlert()
	//if err != nil {
	//	return &pb.AlertResponse{}, err
	//}
	//
	//alerts = append(alerts, unlabeledAlert)

	return &pb.AlertResponse{Alerts: alerts}, nil
}
