package sql

import "github.com/jitsucom/bulker/types"

type SQLTypes map[string]SQLColumn

type SQLColumn struct {
	Type     string `json:"type,omitempty"`
	DdlType  string `json:"ddlType,omitempty"`
	Override bool   `json:"override,omitempty"`
	DataType types.DataType
	// New column represents not commited part of a table schema
	New bool
}

func (c SQLColumn) GetDDLType() string {
	if c.DdlType != "" {
		return c.DdlType
	}
	return c.Type
}

func (s SQLTypes) With(name, sqlType string) SQLTypes {
	return s.WithDDL(name, sqlType, "")
}

func (s SQLTypes) WithDDL(name, sqlType, ddlType string) SQLTypes {
	if sqlType == "" {
		return s
	} else if ddlType == "" {
		s[name] = SQLColumn{Type: sqlType, DdlType: sqlType, Override: true}
	} else {
		s[name] = SQLColumn{Type: sqlType, DdlType: ddlType, Override: true}
	}
	return s
}
