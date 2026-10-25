package schema

// Catalog finds generated tables by table name and by model name. keel gen emits one for every
// table it generates (keeltables.Catalog); NewCatalog builds one from any set of tables.
type Catalog struct {
	tables  []*Table
	byName  map[string]*Table
	byModel map[string]*Table
}

// NewCatalog indexes tables. A model name that names several tables (the same struct name in two
// packages) resolves to none of them.
func NewCatalog(tables []*Table) *Catalog {
	c := &Catalog{
		tables:  tables,
		byName:  make(map[string]*Table, len(tables)),
		byModel: make(map[string]*Table, len(tables)),
	}
	ambiguous := make(map[string]bool)
	for _, t := range tables {
		c.byName[t.Name] = t
		if _, seen := c.byModel[t.Model]; seen {
			ambiguous[t.Model] = true
		}
		c.byModel[t.Model] = t
	}
	for m := range ambiguous {
		delete(c.byModel, m)
	}
	return c
}

// Table returns the table named name.
func (c *Catalog) Table(name string) (*Table, bool) {
	t, ok := c.byName[name]
	return t, ok
}

// Model returns the table of the model named model, false when no table or several have it.
func (c *Catalog) Model(model string) (*Table, bool) {
	t, ok := c.byModel[model]
	return t, ok
}

// Tables returns every table, in the order NewCatalog took them.
func (c *Catalog) Tables() []*Table { return c.tables }
