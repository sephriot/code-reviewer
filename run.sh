export $(grep -v ^# .env | xargs) && code-reviewer
