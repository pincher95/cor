/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package cost provides estimated monthly USD pricing for AWS resources.
// Rates are baked in at compile time; pricing.Refresh() optionally overwrites
// them with live data from the AWS Pricing API.
package cost

import (
	"fmt"
)

// USD is a per-month dollar amount. The string form renders zero/negative
// values as "—" because $0.00 is rarely real cost data (it usually means
// "didn't compute" — surface that explicitly).
type USD float64

func (u USD) String() string {
	switch {
	case u <= 0:
		return "—"
	case u < 1:
		return fmt.Sprintf("$%.2f", float64(u))
	default:
		return fmt.Sprintf("$%.0f", float64(u))
	}
}

// HoursPerMonth is the AWS-standard 730-hour month used by per-hour SKUs
// when projecting to monthly cost.
const HoursPerMonth = 730

// Sum aggregates a slice of USD values.
func Sum(costs []USD) USD {
	var total USD
	for _, c := range costs {
		total += c
	}
	return total
}
