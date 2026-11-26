package cost

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"io"
	"strconv"
	"time"
)

// FOCUS 1.0 columns, including nullable conditional dimensions. Extension columns
// disclose this dataset's internal chargeback methodology and sampling coverage.
var FocusColumns = []string{"AvailabilityZone", "BilledCost", "BillingAccountId", "BillingAccountName", "BillingCurrency", "BillingPeriodEnd", "BillingPeriodStart", "ChargeCategory", "ChargeClass", "ChargeDescription", "ChargeFrequency", "ChargePeriodEnd", "ChargePeriodStart", "CommitmentDiscountCategory", "CommitmentDiscountId", "CommitmentDiscountName", "CommitmentDiscountStatus", "CommitmentDiscountType", "ConsumedQuantity", "ConsumedUnit", "ContractedCost", "ContractedUnitPrice", "EffectiveCost", "InvoiceIssuerName", "ListCost", "ListUnitPrice", "PricingCategory", "PricingQuantity", "PricingUnit", "ProviderName", "PublisherName", "RegionId", "RegionName", "ResourceId", "ResourceName", "ResourceType", "ServiceCategory", "ServiceName", "SkuId", "SkuPriceId", "SubAccountId", "SubAccountName", "Tags", "x_RateCard", "x_Unallocated", "x_SampledHours"}

func FocusCSV(w io.Writer, rows []Row, day time.Time, hours int, installation string) error {
	cw := csv.NewWriter(w)
	if e := cw.Write(FocusColumns); e != nil {
		return e
	}
	day = Day(day)
	month := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, r := range rows {
		tags, e := json.Marshal(r.Tags)
		if e != nil {
			return e
		}
		amount := strconv.FormatFloat(r.Cost, 'f', 10, 64)
		category := r.Category
		if category == "Shared" {
			category = "Management and Governance"
		}
		values := map[string]string{"InvoiceIssuerName": installation, "BilledCost": amount, "EffectiveCost": amount, "ContractedCost": amount, "ListCost": amount, "BillingAccountId": installation, "BillingAccountName": installation, "BillingCurrency": r.Currency, "BillingPeriodStart": month.Format(time.RFC3339), "BillingPeriodEnd": month.AddDate(0, 1, 0).Format(time.RFC3339), "ChargePeriodStart": day.Format(time.RFC3339), "ChargePeriodEnd": day.AddDate(0, 0, 1).Format(time.RFC3339), "ChargeCategory": "Usage", "ChargeDescription": "AEP provisioned capacity chargeback: " + r.Category, "ChargeFrequency": "Recurring", "ConsumedQuantity": "1", "ConsumedUnit": "day", "ContractedUnitPrice": amount, "ListUnitPrice": amount, "PricingCategory": "Standard", "PricingQuantity": "1", "PricingUnit": "day", "ProviderName": "Aenix Enterprise Platform", "PublisherName": "Aenix Enterprise Platform", "ResourceId": r.ResourceID, "ResourceName": r.ResourceName, "ResourceType": r.Category, "ServiceCategory": category, "ServiceName": r.Category, "SkuId": r.RateCard + "/" + r.Category, "SkuPriceId": r.RateCard, "SubAccountId": r.Organization, "SubAccountName": r.Organization, "Tags": string(tags), "x_RateCard": r.RateCard, "x_Unallocated": strconv.FormatBool(r.Unallocated), "x_SampledHours": strconv.Itoa(hours)}
		line := make([]string, len(FocusColumns))
		for i, k := range FocusColumns {
			line[i] = values[k]
		}
		if e = cw.Write(line); e != nil {
			return e
		}
	}
	cw.Flush()
	return cw.Error()
}

type Exporter struct {
	Client               *minio.Client
	Bucket, Installation string
}

func NewExporter(endpoint, bucket, access, secret, region, installation string, secure bool) (*Exporter, error) {
	if endpoint == "" || bucket == "" || access == "" || secret == "" || installation == "" {
		return nil, fmt.Errorf("S3 endpoint, bucket, credentials and installation are required")
	}
	c, e := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Secure: secure, Region: region})
	return &Exporter{c, bucket, installation}, e
}
func (e *Exporter) ExportPending(ctx context.Context, s *Store) error {
	days, err := s.PendingExports(ctx)
	if err != nil {
		return err
	}
	for _, day := range days {
		rows, hours, err := s.Daily(ctx, day)
		if err != nil {
			return err
		}
		var b bytes.Buffer
		if err = FocusCSV(&b, rows, day, hours, e.Installation); err != nil {
			return err
		}
		key := "focus/" + day.Format("2006/01/02") + ".csv"
		if _, err = e.Client.PutObject(ctx, e.Bucket, key, bytes.NewReader(b.Bytes()), int64(b.Len()), minio.PutObjectOptions{ContentType: "text/csv"}); err != nil {
			return err
		}
		if err = s.MarkExported(ctx, day); err != nil {
			return err
		}
	}
	return nil
}
