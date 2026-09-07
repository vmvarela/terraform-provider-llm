// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	tfprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var testMsgAttrTypes = map[string]attr.Type{
	"role":    types.StringType,
	"content": types.StringType,
}

func testMsg(role, content string) types.Object {
	return types.ObjectValueMust(testMsgAttrTypes, map[string]attr.Value{
		"role":    types.StringValue(role),
		"content": types.StringValue(content),
	})
}

func testMsgList(elems ...types.Object) types.List {
	vals := make([]attr.Value, 0, len(elems))
	for _, e := range elems {
		vals = append(vals, e)
	}
	return types.ListValueMust(types.ObjectType{AttrTypes: testMsgAttrTypes}, vals)
}

func TestValidateNonEmptyString(t *testing.T) {
	cases := []struct {
		name     string
		v        types.String
		forApply bool
		wantErr  bool
	}{
		{"value", types.StringValue("gpt-4o-mini"), false, false},
		{"empty", types.StringValue(""), false, true},
		{"whitespace-only", types.StringValue(" \t\n"), false, true},
		{"null", types.StringNull(), false, false},
		{"unknown-plan", types.StringUnknown(), false, false},
		{"unknown-apply", types.StringUnknown(), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validateNonEmptyString("model", tc.v, tc.forApply)
			if got := d.HasError(); got != tc.wantErr {
				t.Fatalf("wantErr=%v, got diags=%v", tc.wantErr, d)
			}
		})
	}
}

func TestValidateFloat64Range(t *testing.T) {
	temperature := []struct {
		v       types.Float64
		wantErr bool
	}{
		{types.Float64Value(0), false},
		{types.Float64Value(2), false},
		{types.Float64Value(1.5), false},
		{types.Float64Value(-0.1), true},
		{types.Float64Value(2.1), true},
		{types.Float64Null(), false},
	}
	for i, tc := range temperature {
		if d := validateFloat64Range("temperature", tc.v, 0, 2, false); d.HasError() != tc.wantErr {
			t.Fatalf("temperature case %d: diags=%v", i, d)
		}
	}
	topP := []struct {
		v       types.Float64
		wantErr bool
	}{
		{types.Float64Value(0), false},
		{types.Float64Value(1), false},
		{types.Float64Value(0.99), false},
		{types.Float64Value(1.1), true},
		{types.Float64Null(), false},
	}
	for i, tc := range topP {
		if d := validateFloat64Range("top_p", tc.v, 0, 1, false); d.HasError() != tc.wantErr {
			t.Fatalf("top_p case %d: diags=%v", i, d)
		}
	}
}

func TestValidatePositiveInt64(t *testing.T) {
	cases := []struct {
		name     string
		v        types.Int64
		forApply bool
		wantErr  bool
	}{
		{"positive", types.Int64Value(1), false, false},
		{"zero", types.Int64Value(0), false, true},
		{"negative", types.Int64Value(-5), false, true},
		{"null", types.Int64Null(), false, false},
		{"unknown-plan", types.Int64Unknown(), false, false},
		{"unknown-apply", types.Int64Unknown(), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validatePositiveInt64("max_tokens", tc.v, tc.forApply)
			if got := d.HasError(); got != tc.wantErr {
				t.Fatalf("wantErr=%v, got diags=%v", tc.wantErr, d)
			}
		})
	}
}

func TestValidateExclusiveInt64(t *testing.T) {
	cases := []struct {
		name     string
		a, b     types.Int64
		forApply bool
		wantErr  bool
	}{
		{"both-set", types.Int64Value(10), types.Int64Value(20), false, true},
		{"a-only", types.Int64Value(10), types.Int64Null(), false, false},
		{"b-only", types.Int64Null(), types.Int64Value(20), false, false},
		{"neither", types.Int64Null(), types.Int64Null(), false, false},
		{"both-unknown-plan", types.Int64Unknown(), types.Int64Unknown(), false, false},
		{"a-unknown-apply", types.Int64Unknown(), types.Int64Value(20), true, false},
		{"both-unknown-apply", types.Int64Unknown(), types.Int64Unknown(), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validateExclusiveInt64("max_tokens", tc.a, "max_completion_tokens", tc.b, tc.forApply)
			if got := d.HasError(); got != tc.wantErr {
				t.Fatalf("wantErr=%v, got diags=%v", tc.wantErr, d)
			}
		})
	}
}

func TestValidateResponseFormat(t *testing.T) {
	cases := []struct {
		name     string
		v        types.String
		forApply bool
		wantErr  bool
	}{
		{"text", types.StringValue("text"), false, false},
		{"json_object", types.StringValue("json_object"), false, false},
		{"invalid", types.StringValue("json"), false, true},
		{"empty", types.StringValue(""), false, true},
		{"null", types.StringNull(), false, false},
		{"unknown-plan", types.StringUnknown(), false, false},
		{"unknown-apply", types.StringUnknown(), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validateResponseFormat(tc.v, tc.forApply)
			if got := d.HasError(); got != tc.wantErr {
				t.Fatalf("wantErr=%v, got diags=%v", tc.wantErr, d)
			}
		})
	}
}

func TestValidateMessages(t *testing.T) {
	ctx := context.Background()
	unknownMsg := types.ObjectUnknown(testMsgAttrTypes)
	unknownRole := types.ObjectValueMust(testMsgAttrTypes, map[string]attr.Value{
		"role": types.StringUnknown(), "content": types.StringValue("hi"),
	})
	unknownContent := types.ObjectValueMust(testMsgAttrTypes, map[string]attr.Value{
		"role": types.StringValue("user"), "content": types.StringUnknown(),
	})

	cases := []struct {
		name     string
		list     types.List
		forApply bool
		wantErr  bool
	}{
		{"null", types.ListNull(types.ObjectType{AttrTypes: testMsgAttrTypes}), false, false},
		{"unknown-plan", types.ListUnknown(types.ObjectType{AttrTypes: testMsgAttrTypes}), false, false},
		{"unknown-apply", types.ListUnknown(types.ObjectType{AttrTypes: testMsgAttrTypes}), true, false},
		{"empty-list-plan", testMsgList(), false, false},
		{"empty-list-apply", testMsgList(), true, false},
		{"valid", testMsgList(testMsg("system", "be brief"), testMsg("user", "hello")), false, false},
		{"whitespace-content-preserved", testMsgList(testMsg("user", "  \n\t ")), false, false},
		{"bad-role", testMsgList(testMsg("tool", "hi")), false, true},
		{"empty-role", testMsgList(testMsg("", "hi")), false, true},
		{"missing-role", testMsgList(types.ObjectValueMust(testMsgAttrTypes, map[string]attr.Value{
			"role": types.StringNull(), "content": types.StringValue("hi"),
		})), false, true},
		{"missing-content", testMsgList(types.ObjectValueMust(testMsgAttrTypes, map[string]attr.Value{
			"role": types.StringValue("user"), "content": types.StringNull(),
		})), false, true},
		{"element-unknown-plan", testMsgList(unknownMsg), false, false},
		{"element-unknown-apply", testMsgList(unknownMsg), true, true},
		{"field-unknown-plan", testMsgList(unknownRole, unknownContent), false, false},
		{"field-unknown-apply", testMsgList(unknownRole, unknownContent), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validateMessages(ctx, tc.list, tc.forApply)
			if got := d.HasError(); got != tc.wantErr {
				t.Fatalf("wantErr=%v, got diags=%v", tc.wantErr, d)
			}
			// Sensitive content must never be echoed in diagnostics.
			for _, x := range d {
				if strings.Contains(x.Detail(), "hello") || strings.Contains(x.Summary(), "hello") {
					t.Fatalf("diagnostic echoes message content: %s: %s", x.Summary(), x.Detail())
				}
			}
		})
	}
}

func TestValidateGenerationNullAndUnknown(t *testing.T) {
	ctx := context.Background()

	allNull := generationResourceModel{
		Model:       types.StringNull(),
		Messages:    types.ListNull(types.ObjectType{AttrTypes: testMsgAttrTypes}),
		Temperature: types.Float64Null(),
	}
	if d := validateGeneration(ctx, allNull, false); d.HasError() {
		t.Fatalf("all-null plan should pass, diags=%v", d)
	}

	allUnknown := generationResourceModel{
		Model:               types.StringUnknown(),
		Messages:            types.ListUnknown(types.ObjectType{AttrTypes: testMsgAttrTypes}),
		ResponseFormat:      types.StringUnknown(),
		Temperature:         types.Float64Unknown(),
		TopP:                types.Float64Unknown(),
		MaxTokens:           types.Int64Unknown(),
		MaxCompletionTokens: types.Int64Unknown(),
	}
	if d := validateGeneration(ctx, allUnknown, false); d.HasError() {
		t.Fatalf("all-unknown plan should pass, diags=%v", d)
	}
	// Apply mode rejects unresolved inputs; nothing sensitive is echoed.
	d := validateGeneration(ctx, allUnknown, true)
	if !d.HasError() {
		t.Fatal("all-unknown apply should fail")
	}
}

func TestGenerationInputsChanged(t *testing.T) {
	msgType := types.ObjectType{AttrTypes: testMsgAttrTypes}
	base := func() generationResourceModel {
		return generationResourceModel{
			Model:       types.StringValue("m1"),
			Messages:    testMsgList(testMsg("user", "hi")),
			Temperature: types.Float64Null(),
		}
	}
	if generationInputsChanged(base(), base()) {
		t.Fatal("identical plans must not require regeneration")
	}
	changed := base()
	changed.Model = types.StringValue("m2")
	if !generationInputsChanged(changed, base()) {
		t.Fatal("model change must require regeneration")
	}
	changed = base()
	changed.Messages = types.ListValueMust(msgType, []attr.Value{
		testMsg("user", "hi"), testMsg("assistant", "ok"),
	})
	if !generationInputsChanged(changed, base()) {
		t.Fatal("messages change must require regeneration")
	}
	changed = base()
	changed.GenerationKey = types.StringValue("v2")
	if !generationInputsChanged(changed, base()) {
		t.Fatal("generation_key change must require regeneration")
	}
}

func TestResourceSchemaAttributes(t *testing.T) {
	ctx := context.Background()
	r := &generationResource{}
	var sresp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sresp)
	if sresp.Diagnostics.HasError() {
		t.Fatalf("schema diags: %v", sresp.Diagnostics)
	}
	attrs := sresp.Schema.Attributes

	required := []string{"model", "messages"}
	for _, n := range required {
		a, ok := attrs[n]
		if !ok || !a.IsRequired() {
			t.Fatalf("%s must be required", n)
		}
	}
	computed := []string{"id", "content", "response_id", "model_used", "finish_reason", "usage"}
	for _, n := range computed {
		a, ok := attrs[n]
		if !ok || !a.IsComputed() {
			t.Fatalf("%s must be computed", n)
		}
	}
	if attrs["content"].IsSensitive() == false {
		t.Fatal("content must be sensitive")
	}
	if attrs["generation_key"] == nil || attrs["generation_key"].IsComputed() {
		t.Fatal("generation_key must be optional, not computed")
	}
	msgs, ok := attrs["messages"].(schema.ListNestedAttribute)
	if !ok || !msgs.Sensitive {
		t.Fatal("messages must be a sensitive ListNestedAttribute")
	}
	if _, ok := msgs.NestedObject.Attributes["content"]; !ok {
		t.Fatal("messages nested object must have content")
	}
	usage, ok := attrs["usage"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("usage must be SingleNestedAttribute")
	}
	for _, n := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		a, ok := usage.Attributes[n]
		if !ok || !a.IsComputed() {
			t.Fatalf("usage.%s must be computed", n)
		}
	}
}

func TestProviderSchemaAttributes(t *testing.T) {
	ctx := context.Background()
	p := New("test")()
	var sresp tfprovider.SchemaResponse
	p.Schema(ctx, tfprovider.SchemaRequest{}, &sresp)
	if sresp.Diagnostics.HasError() {
		t.Fatalf("schema diags: %v", sresp.Diagnostics)
	}
	attrs := sresp.Schema.Attributes
	for _, n := range []string{"base_url", "api_key", "timeout_seconds"} {
		if _, ok := attrs[n]; !ok {
			t.Fatalf("provider schema must have %s", n)
		}
	}
	if attrs["base_url"].IsRequired() {
		t.Fatal("base_url must be optional")
	}
	if attrs["api_key"].IsSensitive() == false {
		t.Fatal("api_key must be sensitive")
	}
}
