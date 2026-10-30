import js from "@eslint/js"
import reactHooks from "eslint-plugin-react-hooks"
import globals from "globals"
import tseslint from "typescript-eslint"

export default tseslint.config(
  { ignores: ["node_modules", "playwright-report", "test-results", "coverage", "../internal/webui/dist"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  reactHooks.configs.flat.recommended,
  {
    languageOptions: { globals: { ...globals.browser, ...globals.node } },
    rules: {
      "@typescript-eslint/no-unused-vars": ["error", { ignoreRestSiblings: true }],
      // Agent and GitHub text is untrusted: never put it in the DOM as HTML.
      "no-restricted-syntax": [
        "error",
        { selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']", message: "Never render untrusted text as HTML." },
        { selector: "AssignmentExpression[left.property.name=/^(innerHTML|outerHTML)$/]", message: "Never render untrusted text as HTML." },
      ],
    },
  },
  { files: ["src/components/ui/**"], rules: { "react-hooks/refs": "off" } },
)
