// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License

#pragma once
// Generated File
// DO NOT EDIT
#include "query/Plan.h"
#include "ExprVisitor.h"

namespace milvus::query {
class ExtractInfoExprVisitor : public ExprVisitor {
 public:
    void
    visit(LogicalUnaryExpr& expr) override;

    void
    visit(LogicalBinaryExpr& expr) override;

    void
    visit(TermExpr& expr) override;

    void
    visit(UnaryRangeExpr& expr) override;

    void
    visit(BinaryArithOpEvalRangeExpr& expr) override;

    void
    visit(BinaryRangeExpr& expr) override;

    void
    visit(CompareExpr& expr) override;

 public:
    explicit ExtractInfoExprVisitor(ExtractedPlanInfo& plan_info) : plan_info_(plan_info) {
    }

 private:
    ExtractedPlanInfo& plan_info_;
};
}  // namespace milvus::query
