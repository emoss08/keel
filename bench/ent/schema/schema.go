// Package schema declares the bench tables for ent. ent keys an entity by one column, so these
// use id alone; the tables' tenant columns are ordinary fields the queries filter on.
package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/lib/pq"
)

// Customer is the customers table.
type Customer struct{ ent.Schema }

func (Customer) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "customers"}}
}

func (Customer) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("organization_id"),
		field.String("business_unit_id"),
		field.String("name"),
		field.String("code"),
		field.String("email").Optional().Nillable(),
		field.Int64("version"),
		field.Int64("created_at"),
		field.Int64("updated_at"),
	}
}

func (Customer) Edges() []ent.Edge {
	return []ent.Edge{edge.To("shipments", Shipment.Type)}
}

// Shipment is the shipments table.
type Shipment struct{ ent.Schema }

func (Shipment) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "shipments"}}
}

func (Shipment) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("organization_id"),
		field.String("business_unit_id"),
		field.String("customer_id"),
		field.String("bill_to_id").Optional().Nillable(),
		field.String("pro_number"),
		field.String("status").SchemaType(map[string]string{dialect.Postgres: "shipment_status"}),
		field.Int16("priority"),
		field.Int64("weight").Optional().Nillable(),
		field.String("total").SchemaType(map[string]string{dialect.Postgres: "numeric(19,4)"}),
		field.Bool("hazmat"),
		field.Other("labels", pq.StringArray{}).SchemaType(map[string]string{dialect.Postgres: "text[]"}),
		field.JSON("attributes", map[string]any{}),
		field.Int16("stage_rank").Immutable(),
		field.Int64("canceled_at").Optional().Nillable(),
		field.Int64("version"),
		field.Int64("created_at"),
		field.Int64("updated_at"),
	}
}

func (Shipment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("customer", Customer.Type).Ref("shipments").Field("customer_id").Unique().Required(),
		edge.To("moves", Move.Type),
	}
}

// Move is the moves table.
type Move struct{ ent.Schema }

func (Move) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "moves"}}
}

func (Move) Fields() []ent.Field {
	return []ent.Field{
		field.String("id"),
		field.String("organization_id"),
		field.String("business_unit_id"),
		field.String("shipment_id"),
		field.Int32("sequence"),
		field.String("status").SchemaType(map[string]string{dialect.Postgres: "shipment_status"}),
		field.Bool("loaded"),
		field.Int64("created_at"),
	}
}

func (Move) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("shipment", Shipment.Type).Ref("moves").Field("shipment_id").Unique().Required(),
	}
}
