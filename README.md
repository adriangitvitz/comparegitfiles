### How to use

Create a diffs.json file at project root level, project must be a git repository

Create an env variable named `GITHUB_TOKEN` with your github token

diffs.json structure: 

- **name**: organization-name/repository-name
- **branch**: branch-name ( not used yet )
- **files**: files to compare if no path is declared
- **ignore**: files to ignore when comparing


Compare files against `files` field in `diffs.json`

```bash
comparegitfiles -compare
```

Compare path against repo

```bash
comparegitfiles -compare -path repo-root-level/path
```

Use `-verbose` to see differences

```bash
comparegitfiles -compare -verbose
```

```bash
comparegitfiles -compare -path repo-root-level/path -verbose
```
