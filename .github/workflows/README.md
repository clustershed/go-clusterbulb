# .github/workflows



---

### release.yaml

Enable release package creation with auto-generate changelog and release notes on new version tag creation

#### generation notes

- ensure main branch is up to date on github for new release
- ensure main branch is up to date in cli
- ehsure github settings/actions workflow permissions are set
- ensure github repository exists in the packages settings

```
git tag -a v0.0.5 -m "v0.0.5"
git push origin v0.0.5
```

