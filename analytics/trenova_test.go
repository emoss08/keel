package analytics_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// trenovaEntity is one entity of a catalog equivalent to the part of Trenova's that its canned
// reports read: its key, its fields as key:type (s string, d decimal, i int, e epoch, b bool;
// string when omitted) and its edges as name>target (to one) or name>>target (to many).
type trenovaEntity struct {
	key, fields, edges string
}

// trenovaEntities is the equivalent catalog. Field types are what keel gen derives from Trenova's
// models with the tag options its port would add (type=epoch on date columns not named *_at).
var trenovaEntities = []trenovaEntity{
	{"shipment", "proNumber bol status totalChargeAmount:d freightChargeAmount:d otherChargeAmount:d pieces:i weight:i createdAt:e billedAt:e actualShipDate:e actualDeliveryDate:e markedReadyToBillAt:e billingTransferStatus",
		"customer>customer serviceType>service_type shipmentType>shipment_type moves>>shipment_move order>order"},
	{"shipment_move", "status sequence:i loaded:b distance:d createdAt:e", "assignment>assignment stops>>stop"},
	{"stop", "type sequence:i pieces:i weight:i actualArrival:e actualDeparture:e scheduledWindowEnd:e", "location>location"},
	{"location", "name city", "locationCategory>location_category"},
	{"location_category", "facilityType", ""},
	{"customer", "name code", ""},
	{"service_type", "code", ""},
	{"shipment_type", "code", ""},
	{"order", "quotedAmount:d totalAmount:d createdAt:e", "customer>customer shipments>>shipment"},
	{"assignment", "status primaryWorkerId trailerId createdAt:e", "tractor>tractor trailer>trailer primaryWorker>worker"},
	{"tractor", "code status make model year:i vin licensePlateNumber registrationNumber registrationExpiry:e",
		"fleetCode>fleet_code equipmentType>equipment_type equipmentManufacturer>equipment_manufacturer primaryWorker>worker secondaryWorker>worker"},
	{"trailer", "code status make model year:i vin licensePlateNumber registrationExpiry:e lastInspectionDate:e maxLoadWeight:i",
		"fleetCode>fleet_code equipmentType>equipment_type equipmentManufacturer>equipment_manufacturer"},
	{"fleet_code", "code", ""},
	{"equipment_type", "code", ""},
	{"equipment_manufacturer", "name", ""},
	{"worker", "firstName lastName status type driverType assignmentBlocked:b availableForDispatch:b canBeAssigned:b",
		"fleetCode>fleet_code position>worker_position profile>worker_profile"},
	{"worker_position", "title department flsaExempt:b isDrivingPosition:b", ""},
	{"worker_profile", "cdlClass cdlRestrictions complianceStatus disqualificationReason endorsement hazmatExpiry:e hireDate:e isQualified:b lastComplianceCheck:e licenseExpiry:e medicalCardExpiry:e mvrDueDate:e physicalDueDate:e terminationDate:e twicExpiry:e", ""},
	{"worker_credential", "number status issuingAuthority issuedAt:e expiresAt:e verifiedAt:e", "worker>worker credentialType>credential_type"},
	{"credential_type", "code name category isRequired:b", ""},
	{"worker_employment_event", "kind workerId effectiveAt:e", ""},
	{"worker_pto_balance", "ptoType workerId accruedYtdDays:d balanceDays:d carriedDays:d usedYtdDays:d", "worker>worker"},
	{"driver_settlement", "settlementNumber status classification payDate:e grossEarningsMinor:i deductionsMinor:i netPayMinor:i reimbursementsMinor:i totalMiles:d", "worker>worker"},
	{"invoice", "number status billType settlementStatus invoiceDate:e dueDate:e totalAmount:d appliedAmount:d otherAmount:d", "customer>customer"},
	{"factoring_company", "name", ""},
	{"factoring_submission", "number status transportTarget createdAt:e sentAt:e fundedAt:e faceMinor:i expectedAdvanceMinor:i expectedFeeMinor:i expectedReserveMinor:i", "factoringCompany>factoring_company"},
	{"factoring_reserve_entry", "invoiceId accountingDate:e cashMinor:i feeMinor:i", "customer>customer factoringCompany>factoring_company"},
	{"factoring_invoice_state", "verdict readySince:e openBalanceMinor:i", "customer>customer factoringCompany>factoring_company invoice>invoice"},
	{"customer_ledger_entry", "documentNumber sourceObjectType transactionDate:e amountMinor:i", "customer>customer"},
	{"accounting_drift_finding", "partyName kind currencyCode status differenceMinor:i", ""},
	{"accounting_sync_record", "objectType status errorCategory queuedAt:e", ""},
	{"gl_account", "accountCode name", "accountType>account_type"},
	{"account_type", "category", ""},
	{"fiscal_period", "name", ""},
	{"fiscal_year", "isCurrent:b", ""},
	{"gl_account_balance", "periodDebitMinor:i periodCreditMinor:i netChangeMinor:i", "glAccount>gl_account fiscalPeriod>fiscal_period fiscalYear>fiscal_year"},
	{"journal_entry", "accountingDate:e isPosted:b", ""},
	{"journal_entry_line", "debitAmount:d creditAmount:d netAmount:d", "glAccount>gl_account journalEntry>journal_entry customer>customer"},
	{"commodity", "name freightClass hazardousMaterialId", "hazardousMaterial>hazardous_material"},
	{"hazardous_material", "class unNumber packingGroup properShippingName placardRequired:b", ""},
	{"shipment_commodity", "pieces:i weight:i", "shipment>shipment commodity>commodity"},
}

// upperCamel turns a key into a Go name: proNumber → ProNumber, shipment_move → ShipmentMove.
func upperCamel(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// fieldColumn returns a schema column and its SQL type for a field spec.
func fieldColumn(spec string) (schema.Column, string) {
	key, typ, _ := strings.Cut(spec, ":")
	info := &schema.ReportInfo{Included: true, Kind: schema.Dimension, Type: schema.ValueString, Aggs: schema.AggsOrdered}
	col := schema.Column{Name: schema.Snake(upperCamel(key)), Field: upperCamel(key), Kind: schema.KindString, Report: info}
	sqlType := "text"
	switch typ {
	case "d":
		info.Kind, info.Type, info.Aggs, col.Kind, sqlType = schema.Measure, schema.ValueDecimal, schema.AggsNumeric, schema.KindOther, "numeric"
	case "i":
		info.Kind, info.Type, info.Aggs, col.Kind, sqlType = schema.Measure, schema.ValueInt, schema.AggsNumeric, schema.KindInt64, "bigint"
	case "e":
		info.Type, info.Aggs, col.Kind, sqlType = schema.ValueEpoch, schema.AggsOrdered, schema.KindInt64, "bigint"
	case "b":
		info.Type, info.Aggs, col.Kind, sqlType = schema.ValueBool, schema.AggsCount, schema.KindBool, "boolean"
	}
	col.Label = upperCamel(key)
	col.Flags = schema.FlagNullable
	return col, sqlType
}

// trenovaTables builds the equivalent catalog's tables and the DDL that creates them.
func trenovaTables() ([]*schema.Table, string) {
	byKey := map[string]*schema.Table{}
	types := map[*schema.Table][]string{}
	var tables []*schema.Table
	for _, e := range trenovaEntities {
		t := &schema.Table{Name: "tr_" + e.key, Model: "trenova." + upperCamel(e.key), Label: upperCamel(e.key),
			Report: &schema.TableReport{Category: "Trenova"}}
		idInfo := &schema.ReportInfo{Included: true, Kind: schema.Dimension, Type: schema.ValueString, Aggs: schema.AggsOrdered}
		t.Columns = []schema.Column{{Name: "id", Field: "ID", Kind: schema.KindString, Flags: schema.FlagPK, Report: idInfo},
			{Name: "organization_id", Field: "OrganizationID", Kind: schema.KindString, Flags: schema.FlagPK | schema.FlagTenant},
			{Name: "business_unit_id", Field: "BusinessUnitID", Kind: schema.KindString, Flags: schema.FlagPK | schema.FlagTenant}}
		types[t] = []string{"text", "text", "text"}
		for _, f := range strings.Fields(e.fields) {
			col, typ := fieldColumn(f)
			t.Columns = append(t.Columns, col)
			types[t] = append(types[t], typ)
		}
		byKey[e.key] = t
		tables = append(tables, t)
	}
	column := func(t *schema.Table, name string) int32 {
		if i := t.Column(name); i >= 0 {
			return i
		}
		t.Columns = append(t.Columns, schema.Column{Name: name, Field: upperCamel(name), Kind: schema.KindString, Flags: schema.FlagNullable})
		types[t] = append(types[t], "text")
		return int32(len(t.Columns) - 1)
	}
	for _, e := range trenovaEntities {
		t := byKey[e.key]
		for _, edge := range strings.Fields(e.edges) {
			var name, target string
			many := false
			if a, b, ok := strings.Cut(edge, ">>"); ok {
				name, target, many = a, b, true
			} else {
				name, target, _ = strings.Cut(edge, ">")
			}
			tt := byKey[target]
			rel := schema.Relation{Name: upperCamel(name), Target: tt, Label: upperCamel(name)}
			if many {
				rel.Kind, rel.Local, rel.Remote = schema.HasMany, []int32{0}, []int32{column(tt, e.key+"_id")}
			} else {
				rel.Kind, rel.Local, rel.Remote = schema.BelongsTo, []int32{column(t, schema.Snake(rel.Name)+"_id")}, []int32{0}
			}
			t.Relations = append(t.Relations, rel)
		}
	}
	ship, move := byKey["shipment"], byKey["shipment_move"]
	for _, last := range []bool{false, true} {
		name := "OriginStop"
		if last {
			name = "DestinationStop"
		}
		ship.Relations = append(ship.Relations, schema.Relation{Name: name, Kind: schema.Pick, Target: byKey["stop"],
			Local: []int32{0}, Path: []int32{ship.Relation("Moves"), move.Relation("Stops")}, Last: last,
			Order: []schema.PickKey{{Hop: 0, Column: move.Column("sequence")}, {Hop: 1, Column: byKey["stop"].Column("sequence")}, {Hop: 1, Column: 0}}})
	}
	var ddl strings.Builder
	for _, t := range tables {
		schema.Derive(t)
		cols := make([]string, len(t.Columns))
		for i, c := range t.Columns {
			cols[i] = c.Quoted + " " + types[t][i]
		}
		fmt.Fprintf(&ddl, "CREATE TABLE IF NOT EXISTS %s (%s, PRIMARY KEY (id, organization_id, business_unit_id));\n",
			t.Quoted, strings.Join(cols, ", "))
	}
	return tables, ddl.String()
}

// trenovaOnce creates the equivalent catalog's tables in the test database once.
var trenovaOnce sync.Once

// trenovaCatalog returns the equivalent catalog over tables created in the test database.
func trenovaCatalog(t *testing.T) *analytics.Catalog {
	t.Helper()
	tables, ddl := trenovaTables()
	var err error
	trenovaOnce.Do(func() { _, err = db(t).Pool().Exec(context.Background(), ddl) })
	if err != nil {
		t.Fatal(err)
	}
	return must[*analytics.Catalog](t)(analytics.Build(tables, analytics.Options{}))
}

func TestTrenovaCannedReports(t *testing.T) {
	cat := trenovaCatalog(t)
	raw, err := os.ReadFile("ir/testdata/trenova/canned.json")
	if err != nil {
		t.Fatal(err)
	}
	var canned map[string]json.RawMessage
	if err := json.Unmarshal(raw, &canned); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(canned))
	for name := range canned {
		names = append(names, name)
	}
	slices.Sort(names)
	r := must[*analytics.Runner](t)(analytics.NewRunner(db(t), cat, analytics.RunnerOptions{}))
	ctx := newTenant()
	var failed []string
	for _, name := range names {
		d, err := ir.Decode(canned[name])
		if err == nil {
			_, err = r.Preview(ctx, analytics.Request{Definition: d})
		}
		if err != nil {
			failed = append(failed, name+": "+err.Error())
		}
	}
	if len(failed) > 0 || len(names) != 43 {
		t.Fatalf("%d of %d canned reports:\n%s", len(failed), len(names), strings.Join(failed, "\n"))
	}
}
