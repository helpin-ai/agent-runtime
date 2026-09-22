# Write clear runtime documentation

Help developers understand agent execution and connect it to their own
applications. Helpin’s README explains the team workspace; this repository
explains the execution service and the responsibilities the application keeps.

- Open with what the reader can do. Use familiar words, concrete verbs, and short
  paragraphs with one main idea.
- Use a real task to explain an interface: an article to review, context to supply,
  or an event to handle. Explain technical terms when they first matter.
- Keep the README focused on understanding and starting. Link to detailed
  configuration, API contracts, and deployment guides.
- Give commands a working directory, prerequisites, and an observable result.
  SDK installation is not service installation. Register any agents an example needs.
- Separate proposed work from approved actions and completed side effects. Say
  which permissions and business decisions belong to the host application.
- Ground behavior in the code and release being documented. Do not turn event
  history into a promise about recovery, isolation, or exactly-once execution.
- Label fictional examples and generated visuals. Never include real credentials
  or customer records. Keep internal review notes out of published copy.

For example: “Your application supplies the article. The agent proposes edits.
Your existing review process decides what to publish.”

Use one descriptive H1, meaningful link labels, and lowercase hyphenated filenames
under `docs/`. Preserve conventional filenames such as `README.md` and
`CONTRIBUTING.md`. Check relative links and heading anchors when moving content.
Run the documented example when changing setup instructions and report any part
that could not be verified, including unavailable provider credentials.
