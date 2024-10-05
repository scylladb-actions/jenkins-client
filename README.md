### A golang-based jenkins client for github actions

This action allows you to interact with a Jenkins server from a GitHub Actions workflow.
Allows you to:
1. Trigger a Jenkins job
2. Wait for a Jenkins job to finish
3. Get the status of a Jenkins job
4. Get the console output of a Jenkins job

## Example

```yaml
jobs:
  run-jenkins-job:
    steps:
      - name: Start Jenkins job
        uses: scylladb-actions/jenkins-client@v0.1.0
        with:
          job_name: 'my_folder/my_job'
          job_parameters: '{"email_recipients": "cicd-results@myorg.com", "param1": "value1"}'
          base_url: 'https://my-jenknins.com/'
          user: ${{ secrets.JENKINS_USERNAME }}
          password: ${{ secrets.JENKINS_TOKEN }}
          wait_timeout: 1h
          polling_interval: 1s
```
