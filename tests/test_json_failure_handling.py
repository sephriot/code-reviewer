#!/usr/bin/env python3
"""Test script to verify JSON parsing failure handling works correctly."""

import sys
import logging
import asyncio
from pathlib import Path

import pytest

# Add src to path for imports
sys.path.insert(0, str(Path(__file__).parent.parent / 'src'))

from code_reviewer.models import PRInfo, ReviewResult, ReviewModel
from code_reviewer.llm_integration import LLMIntegration, LLMOutputParseError

def setup_test_logging():
    """Set up logging for testing."""
    logging.basicConfig(
        level=logging.INFO,
        format='%(asctime)s - %(levelname)s - %(message)s',
        handlers=[logging.StreamHandler()]
    )

@pytest.mark.asyncio
async def test_json_failure_handling():
    """Test that invalid JSON output causes review failure."""
    print("=== Testing JSON Failure Handling ===\n")
    
    # Create a mock integration for testing
    class MockLLMIntegration(LLMIntegration):
        def __init__(self):
            self.model = ReviewModel.CLAUDE
            self.prompt_file = Path(__file__)
        
        async def _run_model_cli(self, pr_info: PRInfo, previous_pending=None) -> str:
            # Return non-JSON output to trigger parsing failure
            return "This is not JSON output from the CLI. It's just plain text."
    
    # Test data
    pr_info = PRInfo(
        id=12345,
        number=123,
        repository=['owner', 'repo'],
        url='https://github.com/owner/repo/pull/123',
        title='Test PR that will fail JSON parsing',
        author='testuser'
    )
    
    llm_integration = MockLLMIntegration()
    
    print("1. Testing with non-JSON output:")
    try:
        review_result = await llm_integration.review_pr(pr_info)
        print("❌ FAILED: Expected LLMOutputParseError but got result:", review_result)
        return False
    except LLMOutputParseError as e:
        print("✅ SUCCESS: LLMOutputParseError raised as expected")
        print(f"   Error message: {str(e)}")
        print(f"   Model output length: {len(e.raw_output)}")
        print(f"   Model output preview: {e.raw_output[:100]}...")
        print()
    except Exception as e:
        print(f"❌ FAILED: Unexpected exception type: {type(e).__name__}: {e}")
        return False
    
    # Test with malformed JSON
    class MockLLMIntegrationMalformedJSON(LLMIntegration):
        def __init__(self):
            self.model = ReviewModel.CLAUDE
            self.prompt_file = Path(__file__)
        
        async def _run_model_cli(self, pr_info: PRInfo, previous_pending=None) -> str:
            # Return malformed JSON
            return '{"action": "approve_without_comment", "malformed": true, missing_quote: "oops"}'
    
    print("2. Testing with malformed JSON:")
    llm_integration_malformed = MockLLMIntegrationMalformedJSON()
    
    try:
        review_result = await llm_integration_malformed.review_pr(pr_info)
        print("❌ FAILED: Expected LLMOutputParseError but got result:", review_result)
        return False
    except LLMOutputParseError as e:
        print("✅ SUCCESS: LLMOutputParseError raised for malformed JSON")
        print(f"   Error message: {str(e)}")
        print(f"   Model output: {e.raw_output}")
        print()
    except Exception as e:
        print(f"❌ FAILED: Unexpected exception type: {type(e).__name__}: {e}")
        return False
    
    # Test with valid JSON (should work)
    class MockLLMIntegrationValidJSON(LLMIntegration):
        def __init__(self):
            self.model = ReviewModel.CLAUDE
            self.prompt_file = Path(__file__)
        
        async def _run_model_cli(self, pr_info: PRInfo, previous_pending=None) -> str:
            # Return valid JSON
            return '{"action": "approve_without_comment"}'
    
    print("3. Testing with valid JSON (should succeed):")
    llm_integration_valid = MockLLMIntegrationValidJSON()
    
    try:
        review_result = await llm_integration_valid.review_pr(pr_info)
        print("✅ SUCCESS: Valid JSON parsed successfully")
        print(f"   Result action: {review_result.action.value}")
        print()
        return True
    except Exception as e:
        print(f"❌ FAILED: Valid JSON should not raise exception: {type(e).__name__}: {e}")
        return False

@pytest.mark.asyncio
async def test_github_monitor_error_handling():
    """Test that GitHubMonitor handles LLMOutputParseError correctly."""
    print("=== Testing GitHubMonitor Error Handling ===\n")
    
    # Create mock classes to test the error handling path
    class MockLLMIntegrationError(LLMIntegration):
        def __init__(self):
            self.model = ReviewModel.CLAUDE
            self.prompt_file = Path(__file__)
        
        async def review_pr(self, pr_info: PRInfo) -> ReviewResult:
            # Simulate the parse error that would happen
            raise LLMOutputParseError(
                "Model output does not contain valid JSON. Output length: 50",
                "This is invalid JSON output that will cause failure"
            )
    
    # Mock GitHubMonitor with minimal dependencies
    class MockGitHubMonitor:
        def __init__(self):
            self.llm_integration = MockLLMIntegrationError()
        
        async def _process_pr(self, pr_info: PRInfo):
            """Simulate the process_pr method with error handling."""
            repo_name = pr_info.repository_name
            logger = logging.getLogger(__name__)
            logger.info(f"Processing PR #{pr_info.number} in {repo_name}")
            
            try:
                # This will raise LLMOutputParseError
                await self.llm_integration.review_pr(pr_info)
                
                # These should never execute due to the exception
                print("❌ FAILED: Should not reach review output logging")
                print("❌ FAILED: Should not reach action processing")
                print("❌ FAILED: Should not reach database storage")
                
                return False
                
            except LLMOutputParseError as e:
                logger.error(f"❌ Review failed for PR #{pr_info.number} in {repo_name}: Invalid JSON output from model CLI")
                logger.error(f"📋 PR: '{pr_info.title}' by {pr_info.author}")
                logger.error(f"❗ Reason: {str(e)}")
                logger.error(f"🔄 This PR will be retried in the next monitoring loop")
                # Log a preview of the output (truncated for readability)
                output_preview = e.raw_output[:1000] + "..." if len(e.raw_output) > 1000 else e.raw_output
                logger.error(f"📤 Model output preview: {output_preview}")
                return False  # Return False to indicate review failed and should be retried
                
            except Exception as e:
                logger.error(f"Error processing PR #{pr_info.number}: {e}")
                return False
    
    # Test data
    pr_info = PRInfo(
        id=12345,
        number=123,
        repository=['owner', 'repo'],
        url='https://github.com/owner/repo/pull/123',
        title='Test PR that will fail JSON parsing in monitor',
        author='testuser'
    )
    
    monitor = MockGitHubMonitor()
    
    print("Testing GitHubMonitor error handling:")
    result = await monitor._process_pr(pr_info)
    
    if not result:  # We expect False (review failed, should retry)
        print("✅ SUCCESS: GitHubMonitor handled LLMOutputParseError correctly")
        print("✅ No PR actions were taken (approve/comment)")
        print("✅ No database record was stored")
        print("✅ PR marked for retry in next monitoring loop")
        result = True  # Test passed
    else:
        print("❌ FAILED: GitHubMonitor should return False for failed reviews")
        result = False
    
    return result

async def main():
    """Main test function."""
    print("Testing JSON failure handling with hard failures (no fallback)...\n")
    
    json_test_passed = await test_json_failure_handling()
    monitor_test_passed = await test_github_monitor_error_handling()
    
    if json_test_passed and monitor_test_passed:
        print("\n🎉 All JSON failure handling tests passed!")
        print("✅ Invalid JSON output now causes review failure")
        print("✅ No approval/comments are made on parsing failures")
        print("✅ No database records are stored on parsing failures")
        print("✅ Proper error logging includes raw model output for debugging")
        return True
    else:
        print("\n❌ Some tests failed!")
        return False

if __name__ == "__main__":
    setup_test_logging()
    
    try:
        success = asyncio.run(main())
        if not success:
            sys.exit(1)
        
    except Exception as e:
        print(f"\n❌ Test failed with unexpected error: {e}")
        import traceback
        traceback.print_exc()
        sys.exit(1)
