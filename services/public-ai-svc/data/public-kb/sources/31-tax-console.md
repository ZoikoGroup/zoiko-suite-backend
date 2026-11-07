---
source_id: kb-tax-console
title: Tax page
domain: Tax
module_id: tax-console
audience: PUBLIC_ANONYMOUS
status: DRAFT_PENDING_REVIEW
owner: owner-product
effective_from: 2026-01-01
expires_at: 2027-12-31
locale: en-US
intent_ids: I17,I02
market_codes: ALL
classification: PUBLIC_APPROVED
pii_state: CLEAR
license: OWNED_PUBLIC
signing_ref: kbctl-dev
---

## Tax page

The Tax page governs direct and indirect tax across entities and jurisdictions. It shows active tax rules, net VAT or GST payable, corporate tax due and upcoming filing deadlines, and has **New Tax Rule**, **Evaluate Determination**, **Assemble Filing** and **Export Report (CSV)** at the top.

## Tax Governance Process Flow

Tax work follows one lifecycle: a tax rule is applied, the tax is determined for a transaction, the return is assembled, the filing is prepared, it is filed with the tax authority, and the tax is settled. Each step on the page can be opened to inspect its records.

## Tax jurisdiction overview

The tax jurisdiction overview shows one card per tax jurisdiction, such as the UK (HMRC), the US (IRS), Singapore (IRAS) and Germany (BZSt): the taxes it covers (VAT, GST, withholding or corporate income tax), how many rules apply, the VAT or GST return status, the filing status and whether the authority connection is active.

## Tax Rules & Determination Engine

The Tax Rules & Determination Engine lists the configured tax rules, each with its jurisdiction, code, name, category, rate, effective date and status, and the tax determinations made for transactions: source, supply, place of supply, gross and taxable amounts, calculated tax, rate and status.

## New Tax Rule

A tax rule sets the rate for a category of tax in one jurisdiction from a date.
To create a tax rule:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Tax & Compliance**, then select **Tax**.
3. At the top of the page, select **New Tax Rule**.
4. Choose the **Jurisdiction**: United Kingdom (HMRC), United States (IRS), Singapore (IRAS) or Germany (BZSt).
5. Set the **Category**, the **Rate** and the **Effective From** date.
6. Review the rule and select **Create Rule**.

## Evaluate Determination

Evaluate Determination works out the tax on a transaction from the rule in force.
To evaluate a tax determination:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Tax & Compliance**, then select **Tax**.
3. At the top of the page, select **Evaluate Determination**.
4. Enter the transaction to evaluate.
5. Select **Run Evaluation**.
The result shows the rule applied, the taxable amount, the rate and the calculated tax.

## Assemble Filing

Assemble Filing puts a tax filing together so it is ready for submission to the authority.
To assemble a filing:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Tax & Compliance**, then select **Tax**.
3. At the top of the page, select **Assemble Filing**.
4. Choose the **Filing Type** (such as VAT100_MTD), the **Period** (such as 2026-Q2) and the **Due Date**.
5. Select **Confirm Assembly**.
The assembled filing is then ready for authority submission.

## Export Report (CSV)

Export Report downloads the tax data shown on the Tax page as a CSV file.
To export the tax report:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Tax & Compliance**, then select **Tax**.
3. At the top of the page, select **Export Report (CSV)**.

## VAT / GST Engine

The VAT / GST Engine runs periodic VAT and GST returns: it computes input and output balances and enforces one-way filing with the tax authority. It has three tabs: **1. Create VAT Return**, **2. File VAT Return** and **3. Lookup & Audit Lineage**.

## Create a VAT return

Creating a VAT return generates a DRAFT return and calculates the net tax payable from output and input tax.
To create a VAT return:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Tax & Compliance**, then select **Tax**.
3. Scroll to the **VAT / GST Engine** and open **1. Create VAT Return**.
4. Enter the **Legal Entity ID**, the **Jurisdiction ID** (such as GB) and the **Tax Registration Number**.
5. Set the **Tax Period** (such as 2026-Q2), the **Reporting Currency** and the **Effective From Date**.
6. Enter the **Total Sales Amount**, **Total Purchases Amount**, **Output Tax Amount** and **Input Tax Amount**.
7. Check the preview, then select **Submit VAT Return (Create Draft)**.
There is one return per entity, jurisdiction and period.

## File a VAT return

Filing moves a VAT return from DRAFT to FILED with the tax authority. The move is one way only.
To file a VAT return:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Tax & Compliance**, then select **Tax**.
3. In the **VAT / GST Engine**, open **2. File VAT Return**.
4. Enter the **VAT Return ID**, or choose a draft from the table.
5. Enter who it is **Filed By (Signatory Officer)**, the authorized filing representative.
6. Submit the filing.

## Look up a VAT return

Lookup & Audit Lineage shows one VAT return's record: its status, period and jurisdiction, net tax payable, and who filed it and when.
To look up a VAT return:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Tax & Compliance**, then select **Tax**.
3. In the **VAT / GST Engine**, open **3. Lookup & Audit Lineage**.
4. Enter the **VAT Return ID** and open the record.

## How net VAT payable is calculated

Net VAT payable is output tax minus input tax: the tax collected on sales minus the tax paid on purchases. For example, output tax of 100000 and input tax of 40000 give a net tax payable of 60000.

## Live VAT / GST Returns Register

The Live VAT / GST Returns Register lists every VAT and GST return with its period, tax registration number, output against input tax, net payable, who filed it and its status: DRAFT, FILED or ACCEPTED. A draft offers **Quick File**, which files it in one step, and every return offers **Audit**, which opens its record and audit lineage.

## Corporate & Withholding Tax

Corporate & Withholding Tax shows corporate income tax returns and withholding tax obligations. Each corporate income tax return runs from gross revenue, less deductions, to taxable income, the tax at the applicable rate, less credits, to net tax payable and the balance due, with its status such as DRAFT or SUBMITTED.

## Withholding tax obligations

Withholding tax obligations list tax withheld at source on payments such as dividends and royalties: the payment reference, type, gross amount, rate, amount withheld, whether a tax treaty applies, and the status, such as CALCULATED or REMITTED.

## Filing Preparation & Authority Interfaces

Filing Preparation & Authority Interfaces shows filing preparation drafts and the connections to tax authorities. Each draft has its filing type (such as VAT100_MTD or US_FORM_1120), period, due date with a countdown or overdue warning, readiness (not started, half-way or ready to file) and validation status (UNVALIDATED, PREPARED or FINALIZED).

## Tax authority connections

Tax authority connections are the secure links used to file with each authority, such as HMRC Making Tax Digital, the US IRS, the Inland Revenue Authority of Singapore and Germany's ELSTER. Each shows its protocol, authentication type (such as OAuth2 or certificate), endpoint and status.
To test a tax authority connection:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Tax & Compliance**, then select **Tax**.
3. Scroll to **Filing Preparation & Authority Interfaces**.
4. In **Tax Authority Connections**, find the authority.
5. Select **Test** on its row.
