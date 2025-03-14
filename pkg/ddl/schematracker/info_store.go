// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package schematracker

import (
	"context"

	"github.com/pingcap/tidb/pkg/infoschema"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/table"
	"github.com/pingcap/tidb/pkg/table/tables"
)

// InfoStore is a simple structure that stores DBInfo and TableInfo. It's modifiable and not thread-safe.
type InfoStore struct {
	lowerCaseTableNames int // same as variable lower_case_table_names

	dbs    map[string]*model.DBInfo
	tables map[string]map[string]*model.TableInfo
}

// NewInfoStore creates a InfoStore.
func NewInfoStore(lowerCaseTableNames int) *InfoStore {
	return &InfoStore{
		lowerCaseTableNames: lowerCaseTableNames,
		dbs:                 map[string]*model.DBInfo{},
		tables:              map[string]map[string]*model.TableInfo{},
	}
}

func (i *InfoStore) ciStr2Key(name ast.CIStr) string {
	if i.lowerCaseTableNames == 0 {
		return name.O
	}
	return name.L
}

// SchemaByName returns the DBInfo of given name. nil if not found.
func (i *InfoStore) SchemaByName(name ast.CIStr) *model.DBInfo {
	key := i.ciStr2Key(name)
	return i.dbs[key]
}

// PutSchema puts a DBInfo, it will overwrite the old one.
func (i *InfoStore) PutSchema(dbInfo *model.DBInfo) {
	key := i.ciStr2Key(dbInfo.Name)
	i.dbs[key] = dbInfo
	if i.tables[key] == nil {
		i.tables[key] = map[string]*model.TableInfo{}
	}
}

// DeleteSchema deletes the schema from InfoSchema. Returns true when the schema exists, false otherwise.
func (i *InfoStore) DeleteSchema(name ast.CIStr) bool {
	key := i.ciStr2Key(name)
	_, ok := i.dbs[key]
	if !ok {
		return false
	}
	delete(i.dbs, key)
	delete(i.tables, key)
	return true
}

// TableByName returns the TableInfo. It will also return the error like an infoschema.
func (i *InfoStore) TableByName(_ context.Context, schema, table ast.CIStr) (*model.TableInfo, error) {
	schemaKey := i.ciStr2Key(schema)
	tables, ok := i.tables[schemaKey]
	if !ok {
		return nil, infoschema.ErrDatabaseNotExists.GenWithStackByArgs(schema)
	}

	tableKey := i.ciStr2Key(table)
	tbl, ok := tables[tableKey]
	if !ok {
		return nil, infoschema.ErrTableNotExists.GenWithStackByArgs(schema, table)
	}
	return tbl, nil
}

// TableClonedByName is like TableByName, plus it will clone the TableInfo.
func (i *InfoStore) TableClonedByName(schema, table ast.CIStr) (*model.TableInfo, error) {
	tbl, err := i.TableByName(context.Background(), schema, table)
	if err != nil {
		return nil, err
	}
	return tbl.Clone(), nil
}

// PutTable puts a TableInfo, it will overwrite the old one. If the schema doesn't exist, it will return ErrDatabaseNotExists.
func (i *InfoStore) PutTable(schemaName ast.CIStr, tblInfo *model.TableInfo) error {
	schemaKey := i.ciStr2Key(schemaName)
	tables, ok := i.tables[schemaKey]
	if !ok {
		return infoschema.ErrDatabaseNotExists.GenWithStackByArgs(schemaName)
	}
	tableKey := i.ciStr2Key(tblInfo.Name)
	tables[tableKey] = tblInfo
	return nil
}

// DeleteTable deletes the TableInfo, it will return ErrDatabaseNotExists or ErrTableNotExists when schema or table does
// not exist.
func (i *InfoStore) DeleteTable(schema, table ast.CIStr) error {
	schemaKey := i.ciStr2Key(schema)
	tables, ok := i.tables[schemaKey]
	if !ok {
		return infoschema.ErrDatabaseNotExists.GenWithStackByArgs(schema)
	}

	tableKey := i.ciStr2Key(table)
	_, ok = tables[tableKey]
	if !ok {
		return infoschema.ErrTableNotExists.GenWithStackByArgs(schema, table)
	}
	delete(tables, tableKey)
	return nil
}

// AllSchemaNames returns all the schemas' names.
func (i *InfoStore) AllSchemaNames() []string {
	names := make([]string, 0, len(i.dbs))
	for name := range i.dbs {
		names = append(names, name)
	}
	return names
}

// AllTableNamesOfSchema return all table names of a schema.
func (i *InfoStore) AllTableNamesOfSchema(schema ast.CIStr) ([]string, error) {
	schemaKey := i.ciStr2Key(schema)
	tables, ok := i.tables[schemaKey]
	if !ok {
		return nil, infoschema.ErrDatabaseNotExists.GenWithStackByArgs(schema)
	}
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	return names, nil
}

// InfoStoreAdaptor convert InfoStore to InfoSchema, it only implements a part of InfoSchema interface to be
// used by DDL interface.
type InfoStoreAdaptor struct {
	infoschema.InfoSchema
	inner *InfoStore
}

// SchemaByName implements the InfoSchema interface.
func (i InfoStoreAdaptor) SchemaByName(schema ast.CIStr) (*model.DBInfo, bool) {
	dbInfo := i.inner.SchemaByName(schema)
	return dbInfo, dbInfo != nil
}

// TableExists implements the InfoSchema interface.
func (i InfoStoreAdaptor) TableExists(schema, table ast.CIStr) bool {
	tableInfo, _ := i.inner.TableByName(context.Background(), schema, table)
	return tableInfo != nil
}

// TableByName implements the InfoSchema interface.
func (i InfoStoreAdaptor) TableByName(ctx context.Context, schema, table ast.CIStr) (t table.Table, err error) {
	tableInfo, err := i.inner.TableByName(ctx, schema, table)
	if err != nil {
		return nil, err
	}
	return tables.MockTableFromMeta(tableInfo), nil
}

// TableInfoByName implements the InfoSchema interface.
func (i InfoStoreAdaptor) TableInfoByName(schema, table ast.CIStr) (*model.TableInfo, error) {
	return i.inner.TableByName(context.Background(), schema, table)
}
