package service

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"billing-service/internal/config"
	"billing-service/internal/model"
	"billing-service/internal/repository"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/option"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/consumption/armconsumption"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

// FinOpsSyncer is responsible for fetching cost data from cloud providers
// (AWS, GCP, Azure) and writing it to the database for AI auditing.
type FinOpsSyncer struct {
	repo repository.Repository
	log  *slog.Logger
	cfg  config.Config
}

// NewFinOpsSyncer creates a new FinOpsSyncer.
func NewFinOpsSyncer(repo repository.Repository, logger *slog.Logger, cfg config.Config) *FinOpsSyncer {
	return &FinOpsSyncer{
		repo: repo,
		log:  logger.With("component", "finops_syncer"),
		cfg:  cfg,
	}
}

// Start begins the background synchronization process.
func (s *FinOpsSyncer) Start(ctx context.Context) {
	s.log.Info("Starting FinOps multi-cloud billing syncer")

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	// Initial sync on startup
	s.syncAllProviders(ctx)

	for {
		select {
		case <-ctx.Done():
			s.log.Info("FinOps syncer shutting down")
			return
		case <-ticker.C:
			s.syncAllProviders(ctx)
		}
	}
}

func (s *FinOpsSyncer) syncAllProviders(ctx context.Context) {
	s.log.Info("Starting daily sync for all cloud providers")

	if err := s.syncAWS(ctx); err != nil {
		s.log.Error("Failed to sync AWS costs", "err", err)
	}

	if err := s.syncGCP(ctx); err != nil {
		s.log.Error("Failed to sync GCP costs", "err", err)
	}

	if err := s.syncAzure(ctx); err != nil {
		s.log.Error("Failed to sync Azure costs", "err", err)
	}
}

// syncAWS fetches costs from AWS Cost Explorer API (T-2 days).
func (s *FinOpsSyncer) syncAWS(ctx context.Context) error {
	s.log.Info("Syncing AWS costs")

	if s.cfg.AWSAccessKeyID == "" || s.cfg.AWSSecretAccessKey == "" {
		s.log.Warn("AWS credentials not provided, skipping AWS sync")
		return nil
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(s.cfg.AWSAccessKeyID, s.cfg.AWSSecretAccessKey, "")),
		awsconfig.WithRegion("us-east-1"), // Cost Explorer is a global service accessible via us-east-1
	)
	if err != nil {
		return err
	}

	client := costexplorer.NewFromConfig(awsCfg)

	// Define T-2 time window
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day()-2, 0, 0, 0, 0, time.UTC)
	end := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, time.UTC)

	startStr := start.Format("2006-01-02")
	endStr := end.Format("2006-01-02")

	metrics := []string{"AmortizedCost"}
	req := &costexplorer.GetCostAndUsageInput{
		TimePeriod: &types.DateInterval{
			Start: aws.String(startStr),
			End:   aws.String(endStr),
		},
		Granularity: types.GranularityDaily,
		Metrics:     metrics,
		GroupBy: []types.GroupDefinition{
			{Type: types.GroupDefinitionTypeDimension, Key: aws.String("SERVICE")},
			{Type: types.GroupDefinitionTypeDimension, Key: aws.String("REGION")},
			{Type: types.GroupDefinitionTypeDimension, Key: aws.String("LINKED_ACCOUNT")},
		},
	}

	res, err := client.GetCostAndUsage(ctx, req)
	if err != nil {
		return err
	}

	for _, resultByTime := range res.ResultsByTime {
		for _, group := range resultByTime.Groups {
			if len(group.Keys) < 3 {
				continue
			}
			serviceName := group.Keys[0]
			region := group.Keys[1]
			accountID := group.Keys[2]

			metric, ok := group.Metrics["AmortizedCost"]
			if !ok || metric.Amount == nil {
				continue
			}

			amount, err := strconv.ParseFloat(*metric.Amount, 64)
			if err != nil || amount == 0 {
				continue
			}

			cost := model.CloudVendorCost{
				Provider:       "aws",
				AccountID:      accountID,
				ServiceName:    serviceName,
				Region:         region,
				UsageStartTime: start,
				UsageEndTime:   end,
				CostAmount:     amount,
				Currency:       aws.ToString(metric.Unit),
			}

			if err := s.repo.UpsertCloudVendorCost(ctx, cost); err != nil {
				s.log.Error("Failed to upsert AWS cost", "err", err, "cost", cost)
			}
		}
	}

	s.log.Info("AWS cost sync completed successfully")
	return nil
}

// syncGCP fetches costs via GCP BigQuery Export.
func (s *FinOpsSyncer) syncGCP(ctx context.Context) error {
	s.log.Info("Syncing GCP costs")

	if s.cfg.GCPCredentialsJSON == "" || s.cfg.GCPBillingProject == "" || s.cfg.GCPBillingDataset == "" || s.cfg.GCPBillingTable == "" {
		s.log.Warn("GCP BigQuery credentials or table info not provided, skipping GCP sync")
		return nil
	}

	client, err := bigquery.NewClient(ctx, s.cfg.GCPBillingProject, option.WithCredentialsJSON([]byte(s.cfg.GCPCredentialsJSON)))
	if err != nil {
		return fmt.Errorf("bigquery.NewClient: %w", err)
	}
	defer client.Close()

	// Define T-2 time window
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day()-2, 0, 0, 0, 0, time.UTC)
	end := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, time.UTC)

	// Build the SQL query for BigQuery
	// Standard GCP billing export schema groups costs by service.description, location.region
	sqlQuery := fmt.Sprintf(`
		SELECT
			service.description as service_name,
			location.region as region,
			billing_account_id as account_id,
			SUM(cost) as total_cost,
			currency
		FROM
			`+"`%s.%s.%s`"+`
		WHERE
			usage_start_time >= @start_time
			AND usage_start_time < @end_time
		GROUP BY
			service_name, region, account_id, currency
	`, s.cfg.GCPBillingProject, s.cfg.GCPBillingDataset, s.cfg.GCPBillingTable)

	q := client.Query(sqlQuery)
	q.Parameters = []bigquery.QueryParameter{
		{Name: "start_time", Value: start},
		{Name: "end_time", Value: end},
	}

	it, err := q.Read(ctx)
	if err != nil {
		return fmt.Errorf("bigquery Read: %w", err)
	}

	type BQRow struct {
		ServiceName string  `bigquery:"service_name"`
		Region      string  `bigquery:"region"`
		AccountID   string  `bigquery:"account_id"`
		TotalCost   float64 `bigquery:"total_cost"`
		Currency    string  `bigquery:"currency"`
	}

	for {
		var row BQRow
		err := it.Next(&row)
		if err != nil {
			// iterator.Done is returned when there are no more rows
			if err.Error() == "no more items in iterator" {
				break
			}
			return fmt.Errorf("bigquery Next: %w", err)
		}

		if row.TotalCost == 0 {
			continue
		}

		// Clean up empty fields
		region := row.Region
		if region == "" {
			region = "global"
		}

		cost := model.CloudVendorCost{
			Provider:       "gcp",
			AccountID:      row.AccountID,
			ServiceName:    row.ServiceName,
			Region:         region,
			UsageStartTime: start,
			UsageEndTime:   end,
			CostAmount:     row.TotalCost,
			Currency:       row.Currency,
		}

		if err := s.repo.UpsertCloudVendorCost(ctx, cost); err != nil {
			s.log.Error("Failed to upsert GCP cost", "err", err, "cost", cost)
		}
	}

	s.log.Info("GCP cost sync completed successfully")
	return nil
}

// syncAzure fetches costs via Azure Cost Management API.
func (s *FinOpsSyncer) syncAzure(ctx context.Context) error {
	s.log.Info("Syncing Azure costs")

	if s.cfg.AzureTenantID == "" || s.cfg.AzureClientID == "" || s.cfg.AzureClientSecret == "" || s.cfg.AzureSubscriptionID == "" {
		s.log.Warn("Azure credentials not provided, skipping Azure sync")
		return nil
	}

	cred, err := azidentity.NewClientSecretCredential(s.cfg.AzureTenantID, s.cfg.AzureClientID, s.cfg.AzureClientSecret, nil)
	if err != nil {
		return fmt.Errorf("azure identity: %w", err)
	}

	clientFactory, err := armconsumption.NewClientFactory(s.cfg.AzureSubscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("azure client factory: %w", err)
	}
	usageClient := clientFactory.NewUsageDetailsClient()

	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day()-2, 0, 0, 0, 0, time.UTC)
	end := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, time.UTC)

	startStr := start.Format("2006-01-02")
	endStr := end.Format("2006-01-02")

	// Azure usage details filter format
	filter := fmt.Sprintf("properties/usageStart ge '%s' and properties/usageEnd le '%s'", startStr, endStr)
	expand := ""
	skiptoken := ""
	top := int32(1000)

	pager := usageClient.NewListPager("subscriptions/"+s.cfg.AzureSubscriptionID, &armconsumption.UsageDetailsClientListOptions{
		Expand:    &expand,
		Filter:    &filter,
		Skiptoken: &skiptoken,
		Top:       &top,
		Metric:    nil,
	})

	// To group by service and region, we aggregate locally since the UsageDetails API returns flat rows.
	type key struct {
		Service string
		Region  string
	}
	aggs := make(map[key]float64)

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("azure NextPage: %w", err)
		}
		for _, v := range page.Value {
			if v == nil {
				continue
			}

			// armconsumption.UsageDetailClassification interface allows type switching
			switch prop := v.(type) {
			case *armconsumption.LegacyUsageDetail:
				if prop.Properties == nil || prop.Properties.Cost == nil {
					continue
				}
				amount := *prop.Properties.Cost
				if amount == 0 {
					continue
				}
				
				svc := "Unknown"
				if prop.Properties.MeterDetails != nil && prop.Properties.MeterDetails.MeterCategory != nil {
					svc = *prop.Properties.MeterDetails.MeterCategory
				}
				loc := "Unknown"
				if prop.Properties.ResourceLocation != nil {
					loc = *prop.Properties.ResourceLocation
				}
				
				aggs[key{Service: svc, Region: loc}] += amount
				
			case *armconsumption.ModernUsageDetail:
				if prop.Properties == nil || prop.Properties.CostInBillingCurrency == nil {
					continue
				}
				amount := *prop.Properties.CostInBillingCurrency
				if amount == 0 {
					continue
				}
				
				svc := "Unknown"
				if prop.Properties.MeterCategory != nil {
					svc = *prop.Properties.MeterCategory
				}
				loc := "Unknown"
				if prop.Properties.ResourceLocation != nil {
					loc = *prop.Properties.ResourceLocation
				}
				
				aggs[key{Service: svc, Region: loc}] += amount
			}
		}
	}

	for k, totalCost := range aggs {
		cost := model.CloudVendorCost{
			Provider:       "azure",
			AccountID:      s.cfg.AzureSubscriptionID,
			ServiceName:    k.Service,
			Region:         k.Region,
			UsageStartTime: start,
			UsageEndTime:   end,
			CostAmount:     totalCost,
			Currency:       "USD", // Defaulting to USD for simplification, though ModernUsageDetail provides it
		}

		if err := s.repo.UpsertCloudVendorCost(ctx, cost); err != nil {
			s.log.Error("Failed to upsert Azure cost", "err", err, "cost", cost)
		}
	}

	s.log.Info("Azure cost sync completed successfully")
	return nil
}
