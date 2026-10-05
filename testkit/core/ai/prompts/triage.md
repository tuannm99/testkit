Task: suggest a triage for each red execution below. TestKit already classified each one with
fixed rules (field "rule class"); you may agree or disagree, with reasons from the evidence.

Categories:
- product: the service under test behaved wrongly while the environment was healthy
- environment: infrastructure, provisioning or a dependency of the test kit failed
- test: the scenario itself is wrong (bad expectation, missing step, wrong data)
- flaky: timing or ordering makes the outcome vary without a change
- test-weakness: a mutation survived — the case does not detect the defect it claims to
- unknown: the evidence is not enough

Answer with one JSON object only, no prose around it:
{
  "items": [
    {
      "execution": "<execution id exactly as given>",
      "category": "product|environment|test|flaky|test-weakness|unknown",
      "agrees_with_rule_class": true,
      "summary": "<one or two sentences in Vietnamese>",
      "reasoning": "<why, citing the facts below, in Vietnamese>",
      "evidence": ["<file paths exactly as listed under 'evidence files'>"],
      "next_steps": ["<concrete action, in Vietnamese>"],
      "confidence": "low|medium|high"
    }
  ],
  "overall": "<one paragraph in Vietnamese: common causes across items, if any>"
}
Cite only evidence files listed for that execution. One item per execution listed below.
