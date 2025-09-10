"""Web UI server for managing PR reviews and approvals."""

import json
import logging
from pathlib import Path
from typing import Dict, Any, Optional, List

from fastapi import FastAPI, HTTPException, Request, Form
from fastapi.responses import HTMLResponse, JSONResponse
from fastapi.staticfiles import StaticFiles
from fastapi.templating import Jinja2Templates

from .database import ReviewDatabase
from .github_client import GitHubClient
from .models import ReviewRecord, ReviewResult, ReviewAction, InlineComment, PRInfo


logger = logging.getLogger(__name__)


class ReviewWebServer:
    """Web server for PR review management."""

    def __init__(self, database: ReviewDatabase, github_client: GitHubClient):
        self.database = database
        self.github_client = github_client
        self.app = FastAPI(title="Code Review Dashboard")
        
        # Setup static files and templates
        self.templates_dir = Path(__file__).parent / "templates"
        self.static_dir = Path(__file__).parent / "static"
        self.templates_dir.mkdir(exist_ok=True)
        self.static_dir.mkdir(exist_ok=True)
        
        # Create default templates if they don't exist
        self._create_default_templates()
        
        # Setup templates and static files
        self.templates = Jinja2Templates(directory=str(self.templates_dir))
        self.app.mount("/static", StaticFiles(directory=str(self.static_dir)), name="static")
        
        self._setup_routes()
    
    def _create_default_templates(self):
        """Create default HTML templates."""
        # Dashboard template
        dashboard_html = """<!DOCTYPE html>
<html>
<head>
    <title>Code Review Dashboard</title>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, sans-serif; margin: 20px; }
        .container { max-width: 1200px; margin: 0 auto; }
        .section { margin-bottom: 30px; }
        .pr-card { border: 1px solid #ddd; border-radius: 8px; padding: 15px; margin-bottom: 15px; }
        .pr-title { font-size: 18px; font-weight: bold; margin-bottom: 10px; }
        .pr-meta { color: #666; margin-bottom: 10px; }
        .review-comment { background: #f8f9fa; padding: 10px; border-radius: 4px; margin: 10px 0; }
        .original-review-comment { background: #e9ecef; padding: 10px; border-radius: 4px; margin: 10px 0; border-left: 3px solid #6c757d; }
        .inline-comments { margin-top: 10px; }
        .inline-comment { background: #fff3cd; padding: 8px; border-left: 3px solid #ffc107; margin: 5px 0; }
        .buttons { margin-top: 15px; }
        .btn { padding: 8px 16px; border: none; border-radius: 4px; cursor: pointer; margin-right: 10px; }
        .btn-approve { background: #28a745; color: white; }
        .btn-reject { background: #dc3545; color: white; }
        .btn-view { background: #17a2b8; color: white; }
        .tabs { border-bottom: 1px solid #ddd; margin-bottom: 20px; }
        .tab { display: inline-block; padding: 10px 20px; cursor: pointer; border-bottom: 2px solid transparent; }
        .tab.active { border-bottom-color: #007bff; color: #007bff; }
        .tab-content { display: none; }
        .tab-content.active { display: block; }
        .modal { display: none; position: fixed; z-index: 1000; left: 0; top: 0; width: 100%; height: 100%; background: rgba(0,0,0,0.5); }
        .modal-content { background: white; margin: 5% auto; padding: 20px; width: 80%; max-width: 600px; border-radius: 8px; }
        .close { float: right; font-size: 28px; font-weight: bold; cursor: pointer; }
        textarea { width: 100%; height: 100px; margin: 10px 0; padding: 10px; border: 1px solid #ddd; border-radius: 4px; }
    </style>
</head>
<body>
    <div class="container">
        <h1>Code Review Dashboard</h1>
        
        <div class="tabs">
            <div class="tab active" onclick="showTab('pending')">Pending Approvals</div>
            <div class="tab" onclick="showTab('human-review')">Human Reviews</div>
        </div>
        
        <div id="pending" class="tab-content active">
            <div class="section">
                <h2>PRs Awaiting Your Approval</h2>
                <div id="pending-approvals"></div>
            </div>
        </div>
        
        <div id="human-review" class="tab-content">
            <div class="section">
                <h2>PRs Requiring Human Review</h2>
                <div id="human-reviews"></div>
            </div>
        </div>
    </div>

    <!-- Modal for approval confirmation -->
    <div id="approval-modal" class="modal">
        <div class="modal-content">
            <span class="close" onclick="closeModal()">&times;</span>
            <h3>Review Approval</h3>
            <div id="modal-pr-info"></div>
            <textarea id="user-comment" placeholder="Optional: Modify the review comment before posting..."></textarea>
            <div class="buttons">
                <button class="btn btn-approve" onclick="confirmApproval()">Approve & Post Review</button>
                <button class="btn btn-reject" onclick="rejectApproval()">Reject</button>
            </div>
        </div>
    </div>

    <script>
        let currentApprovalId = null;

        function showTab(tabName) {
            // Hide all tabs
            document.querySelectorAll('.tab-content').forEach(content => content.classList.remove('active'));
            document.querySelectorAll('.tab').forEach(tab => tab.classList.remove('active'));
            
            // Show selected tab
            document.getElementById(tabName).classList.add('active');
            event.target.classList.add('active');
            
            // Load data for the tab
            if (tabName === 'pending') {
                loadPendingApprovals();
            } else if (tabName === 'human-review') {
                loadHumanReviews();
            }
        }

        async function loadPendingApprovals() {
            try {
                const response = await fetch('/api/pending-approvals');
                const approvals = await response.json();
                const container = document.getElementById('pending-approvals');
                
                if (approvals.length === 0) {
                    container.innerHTML = '<p>No pending approvals.</p>';
                    return;
                }
                
                container.innerHTML = approvals.map(approval => `
                    <div class="pr-card">
                        <div class="pr-title">${approval.pr_title}</div>
                        <div class="pr-meta">
                            ${approval.repository} #${approval.pr_number} by ${approval.pr_author}
                            <br><small>Created: ${new Date(approval.created_at).toLocaleString()}</small>
                        </div>
                        ${approval.display_review_comment ? `
                            <div class="review-comment" id="comment-section-${approval.id}">
                                <strong>Review Comment:</strong>
                                <button class="btn-small edit-btn" onclick="editComment(${approval.id})" style="margin-left: 10px; font-size: 12px; padding: 2px 6px;">Edit</button>
                                <button class="btn-small delete-btn" onclick="deleteComment(${approval.id})" style="margin-left: 5px; font-size: 12px; padding: 2px 6px; background: #dc3545;">Delete</button>
                                <div id="comment-display-${approval.id}" class="comment-display">${approval.display_review_comment}</div>
                                <textarea id="comment-edit-${approval.id}" class="comment-editor" style="display: none; width: 100%; height: 100px; margin: 10px 0;">${approval.display_review_comment}</textarea>
                                <div id="comment-edit-buttons-${approval.id}" style="display: none; margin-top: 5px;">
                                    <button class="btn btn-approve" onclick="saveComment(${approval.id})" style="margin-right: 5px;">Save</button>
                                    <button class="btn" onclick="cancelEditComment(${approval.id})">Cancel</button>
                                </div>
                            </div>` : ''}
                        ${approval.display_review_summary ? `
                            <div class="review-summary" id="summary-section-${approval.id}">
                                <strong>Review Summary:</strong>
                                <button class="btn-small edit-btn" onclick="editSummary(${approval.id})" style="margin-left: 10px; font-size: 12px; padding: 2px 6px;">Edit</button>
                                <button class="btn-small delete-btn" onclick="deleteSummary(${approval.id})" style="margin-left: 5px; font-size: 12px; padding: 2px 6px; background: #dc3545;">Delete</button>
                                <div id="summary-display-${approval.id}" class="summary-display">${approval.display_review_summary}</div>
                                <textarea id="summary-edit-${approval.id}" class="summary-editor" style="display: none; width: 100%; height: 100px; margin: 10px 0;">${approval.display_review_summary}</textarea>
                                <div id="summary-edit-buttons-${approval.id}" style="display: none; margin-top: 5px;">
                                    <button class="btn btn-approve" onclick="saveSummary(${approval.id})" style="margin-right: 5px;">Save</button>
                                    <button class="btn" onclick="cancelEditSummary(${approval.id})">Cancel</button>
                                </div>
                            </div>` : ''}
                        ${approval.inline_comments.length > 0 ? `
                            <div class="inline-comments">
                                <strong>Inline Comments (${approval.inline_comments.length}):</strong>
                                ${approval.inline_comments.map(comment => `
                                    <div class="inline-comment">
                                        <strong>${comment.file}:${comment.line}</strong><br>
                                        ${comment.message}
                                    </div>
                                `).join('')}
                            </div>
                        ` : ''}
                        <div class="buttons">
                            <button class="btn btn-approve" onclick="approveDirectly(${approval.id})">
                                Approve & Post Review
                            </button>
                            <button class="btn btn-reject" onclick="rejectPR(${approval.id})">Reject</button>
                            <a href="${approval.pr_url}" target="_blank" class="btn btn-view">View PR</a>
                        </div>
                    </div>
                `).join('');
            } catch (error) {
                console.error('Failed to load pending approvals:', error);
            }
        }

        async function loadHumanReviews() {
            try {
                const response = await fetch('/api/human-reviews');
                const reviews = await response.json();
                const container = document.getElementById('human-reviews');
                
                if (reviews.length === 0) {
                    container.innerHTML = '<p>No PRs marked for human review.</p>';
                    return;
                }
                
                container.innerHTML = reviews.map(review => `
                    <div class="pr-card">
                        <div class="pr-title">${review.pr_title}</div>
                        <div class="pr-meta">
                            ${review.repository} #${review.pr_number} by ${review.pr_author}
                            <br><small>Reviewed: ${new Date(review.reviewed_at).toLocaleString()}</small>
                        </div>
                        ${review.review_reason ? `<div class="review-comment"><strong>Reason:</strong><br>${review.review_reason}</div>` : ''}
                        <div class="buttons">
                            <a href="https://github.com/${review.repository}/pull/${review.pr_number}" target="_blank" class="btn btn-view">View PR</a>
                        </div>
                    </div>
                `).join('');
            } catch (error) {
                console.error('Failed to load human reviews:', error);
            }
        }

        function showApprovalModal(id, title, comment) {
            console.log('showApprovalModal called with:', id, title, comment);
            currentApprovalId = id;
            document.getElementById('modal-pr-info').innerHTML = `<strong>${title}</strong>`;
            document.getElementById('user-comment').value = comment;
            document.getElementById('approval-modal').style.display = 'block';
            console.log('Modal displayed, currentApprovalId:', currentApprovalId);
        }

        function closeModal() {
            document.getElementById('approval-modal').style.display = 'none';
            currentApprovalId = null;
        }

        async function confirmApproval() {
            console.log('confirmApproval called, currentApprovalId:', currentApprovalId);
            if (!currentApprovalId) {
                console.log('No currentApprovalId, returning');
                return;
            }
            
            const userComment = document.getElementById('user-comment').value;
            console.log('User comment:', userComment);
            
            try {
                console.log('Sending approval request to:', `/api/approvals/${currentApprovalId}/approve`);
                const response = await fetch(`/api/approvals/${currentApprovalId}/approve`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ comment: userComment })
                });
                
                console.log('Response received:', response.status, response.ok);
                
                if (response.ok) {
                    closeModal();
                    loadPendingApprovals();
                    alert('PR approved and review posted!');
                } else {
                    const errorText = await response.text();
                    console.error('Server error:', errorText);
                    alert('Failed to approve PR: ' + errorText);
                }
            } catch (error) {
                console.error('Failed to approve PR:', error);
                alert('Failed to approve PR: ' + error.message);
            }
        }

        async function rejectApproval() {
            if (!currentApprovalId) return;
            
            const reason = prompt('Optional: Why are you rejecting this approval?');
            
            try {
                const response = await fetch(`/api/approvals/${currentApprovalId}/reject`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ reason: reason || '' })
                });
                
                if (response.ok) {
                    closeModal();
                    loadPendingApprovals();
                    alert('Approval rejected');
                } else {
                    alert('Failed to reject approval');
                }
            } catch (error) {
                console.error('Failed to reject approval:', error);
                alert('Failed to reject approval');
            }
        }

        async function rejectPR(id) {
            const reason = prompt('Optional: Why are you rejecting this approval?');
            
            try {
                const response = await fetch(`/api/approvals/${id}/reject`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ reason: reason || '' })
                });
                
                if (response.ok) {
                    loadPendingApprovals();
                    alert('Approval rejected');
                } else {
                    alert('Failed to reject approval');
                }
            } catch (error) {
                console.error('Failed to reject approval:', error);
                alert('Failed to reject approval');
            }
        }

        // Inline editing functions
        function editComment(id) {
            document.getElementById(`comment-display-${id}`).style.display = 'none';
            document.getElementById(`comment-edit-${id}`).style.display = 'block';
            document.getElementById(`comment-edit-buttons-${id}`).style.display = 'block';
        }

        function cancelEditComment(id) {
            document.getElementById(`comment-display-${id}`).style.display = 'block';
            document.getElementById(`comment-edit-${id}`).style.display = 'none';
            document.getElementById(`comment-edit-buttons-${id}`).style.display = 'none';
            // Reset textarea to original value
            const displayText = document.getElementById(`comment-display-${id}`).textContent;
            document.getElementById(`comment-edit-${id}`).value = displayText;
        }

        async function saveComment(id) {
            const newComment = document.getElementById(`comment-edit-${id}`).value;
            try {
                const response = await fetch(`/api/approvals/${id}/update-comment`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ comment: newComment })
                });
                
                if (response.ok) {
                    document.getElementById(`comment-display-${id}`).textContent = newComment;
                    cancelEditComment(id);
                } else {
                    alert('Failed to save comment');
                }
            } catch (error) {
                console.error('Failed to save comment:', error);
                alert('Failed to save comment');
            }
        }

        async function deleteComment(id) {
            if (!confirm('Delete this comment? It will be removed from the review.')) return;
            
            try {
                const response = await fetch(`/api/approvals/${id}/delete-comment`, {
                    method: 'POST'
                });
                
                if (response.ok) {
                    document.getElementById(`comment-section-${id}`).style.display = 'none';
                } else {
                    alert('Failed to delete comment');
                }
            } catch (error) {
                console.error('Failed to delete comment:', error);
                alert('Failed to delete comment');
            }
        }

        function editSummary(id) {
            document.getElementById(`summary-display-${id}`).style.display = 'none';
            document.getElementById(`summary-edit-${id}`).style.display = 'block';
            document.getElementById(`summary-edit-buttons-${id}`).style.display = 'block';
        }

        function cancelEditSummary(id) {
            document.getElementById(`summary-display-${id}`).style.display = 'block';
            document.getElementById(`summary-edit-${id}`).style.display = 'none';
            document.getElementById(`summary-edit-buttons-${id}`).style.display = 'none';
            // Reset textarea to original value
            const displayText = document.getElementById(`summary-display-${id}`).textContent;
            document.getElementById(`summary-edit-${id}`).value = displayText;
        }

        async function saveSummary(id) {
            const newSummary = document.getElementById(`summary-edit-${id}`).value;
            try {
                const response = await fetch(`/api/approvals/${id}/update-summary`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ summary: newSummary })
                });
                
                if (response.ok) {
                    document.getElementById(`summary-display-${id}`).textContent = newSummary;
                    cancelEditSummary(id);
                } else {
                    alert('Failed to save summary');
                }
            } catch (error) {
                console.error('Failed to save summary:', error);
                alert('Failed to save summary');
            }
        }

        async function deleteSummary(id) {
            if (!confirm('Delete this summary? It will be removed from the review.')) return;
            
            try {
                const response = await fetch(`/api/approvals/${id}/delete-summary`, {
                    method: 'POST'
                });
                
                if (response.ok) {
                    document.getElementById(`summary-section-${id}`).style.display = 'none';
                } else {
                    alert('Failed to delete summary');
                }
            } catch (error) {
                console.error('Failed to delete summary:', error);
                alert('Failed to delete summary');
            }
        }

        async function approveDirectly(id) {
            if (!confirm('Approve and post this review to GitHub?')) return;
            
            // Get current comment text (visible or empty if deleted)
            const commentSection = document.getElementById(`comment-section-${id}`);
            const commentText = commentSection && commentSection.style.display !== 'none' ? 
                document.getElementById(`comment-display-${id}`).textContent : '';
            
            try {
                const response = await fetch(`/api/approvals/${id}/approve`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ comment: commentText })
                });
                
                if (response.ok) {
                    loadPendingApprovals();
                    alert('PR approved and review posted!');
                } else {
                    const errorText = await response.text();
                    alert('Failed to approve PR: ' + errorText);
                }
            } catch (error) {
                console.error('Failed to approve PR:', error);
                alert('Failed to approve PR: ' + error.message);
            }
        }

        // Load initial data
        loadPendingApprovals();
    </script>
</body>
</html>"""
        
        dashboard_path = self.templates_dir / "dashboard.html"
        if not dashboard_path.exists():
            dashboard_path.write_text(dashboard_html, encoding='utf-8')
    
    def _setup_routes(self):
        """Setup FastAPI routes."""
        
        @self.app.get("/", response_class=HTMLResponse)
        async def dashboard(request: Request):
            """Main dashboard page."""
            return self.templates.TemplateResponse("dashboard.html", {"request": request})
        
        @self.app.get("/api/pending-approvals")
        async def get_pending_approvals():
            """Get all pending approvals."""
            try:
                approvals = await self.database.get_pending_approvals()
                return JSONResponse(content=approvals)
            except Exception as e:
                logger.error(f"Failed to get pending approvals: {e}")
                raise HTTPException(status_code=500, detail=str(e))
        
        @self.app.get("/api/human-reviews")
        async def get_human_reviews():
            """Get PRs marked for human review."""
            try:
                reviews = await self.database.get_human_review_prs()
                return JSONResponse(content=[{
                    "id": review.id,
                    "repository": review.repository,
                    "pr_number": review.pr_number,
                    "pr_title": review.pr_title,
                    "pr_author": review.pr_author,
                    "review_reason": review.review_reason,
                    "reviewed_at": review.reviewed_at
                } for review in reviews])
            except Exception as e:
                logger.error(f"Failed to get human reviews: {e}")
                raise HTTPException(status_code=500, detail=str(e))
        
        @self.app.post("/api/approvals/{approval_id}/approve")
        async def approve_pr(approval_id: int, request: Request):
            """Approve a PR and post the review."""
            try:
                body = await request.json()
                user_comment = body.get('comment', '')
                logger.info(f"Received approval request for ID {approval_id} with comment: '{user_comment}'")
                
                # Get the pending approval
                approval = await self.database.get_pending_approval(approval_id)
                if not approval:
                    raise HTTPException(status_code=404, detail="Approval not found")
                
                if approval['status'] != 'pending':
                    raise HTTPException(status_code=400, detail="Approval is not pending")
                
                # Create PR info and review result objects
                pr_info = PRInfo(
                    id=0,  # Not used for this purpose
                    number=approval['pr_number'],
                    repository=approval['repository'].split('/'),
                    url=approval['pr_url'],
                    title=approval['pr_title'],
                    author=approval['pr_author']
                )
                
                # Recreate inline comments
                inline_comments = [
                    InlineComment(
                        file=comment['file'],
                        line=comment['line'],
                        message=comment['message']
                    )
                    for comment in approval['inline_comments']
                ]
                
                # Use edited versions if available, otherwise use originals
                final_comment = user_comment or approval['display_review_comment']
                final_summary = approval['display_review_summary']
                
                review_result = ReviewResult(
                    action=ReviewAction(approval['review_action']),
                    comment=final_comment,
                    summary=final_summary,
                    reason=approval['review_reason'],
                    comments=inline_comments  # Already using edited comments from approval['inline_comments']
                )
                
                # Post the review to GitHub
                logger.info(f"Attempting to post GitHub review for PR #{pr_info.number} in {pr_info.repository_name}")
                logger.info(f"Review action: {review_result.action}, comment: '{review_result.comment}', inline comments: {len(review_result.comments)}")
                for i, comment in enumerate(review_result.comments):
                    logger.info(f"  Inline comment {i}: {comment.file}:{comment.line} - {comment.message[:100]}...")
                success = await self._post_github_review(pr_info, review_result)
                logger.info(f"GitHub review post result: {success}")
                
                if success:
                    # Update approval status
                    await self.database.update_pending_approval_status(
                        approval_id, 'approved', user_comment
                    )
                    
                    # Record the review in main reviews table
                    await self.database.record_review(pr_info, review_result)
                    
                    logger.info(f"Successfully approved PR {approval_id}")
                    return JSONResponse(content={"status": "success"})
                else:
                    logger.error(f"Failed to post GitHub review for approval {approval_id}")
                    raise HTTPException(status_code=500, detail="Failed to post GitHub review")
                    
            except Exception as e:
                logger.error(f"Failed to approve PR {approval_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))
        
        @self.app.post("/api/approvals/{approval_id}/reject")
        async def reject_pr(approval_id: int, request: Request):
            """Reject a pending approval."""
            try:
                body = await request.json()
                reason = body.get('reason', '')
                
                # Update approval status
                success = await self.database.update_pending_approval_status(
                    approval_id, 'rejected', reason
                )
                
                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval not found")
                    
            except Exception as e:
                logger.error(f"Failed to reject PR {approval_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))
        
        @self.app.get("/api/stats")
        async def get_stats():
            """Get review statistics."""
            try:
                stats = await self.database.get_review_stats()
                return JSONResponse(content=stats)
            except Exception as e:
                logger.error(f"Failed to get stats: {e}")
                raise HTTPException(status_code=500, detail=str(e))
        
        # Edit/Delete API endpoints
        @self.app.post("/api/approvals/{approval_id}/update-comment")
        async def update_approval_comment(approval_id: int, request: Request):
            """Update the review comment for a pending approval."""
            try:
                body = await request.json()
                new_comment = body.get('comment', '')
                
                success = await self.database.update_approval_comment(approval_id, new_comment)
                
                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval not found")
                    
            except Exception as e:
                logger.error(f"Failed to update comment for approval {approval_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))
        
        @self.app.post("/api/approvals/{approval_id}/update-summary")
        async def update_approval_summary(approval_id: int, request: Request):
            """Update the review summary for a pending approval."""
            try:
                body = await request.json()
                new_summary = body.get('summary', '')
                
                success = await self.database.update_approval_summary(approval_id, new_summary)
                
                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval not found")
                    
            except Exception as e:
                logger.error(f"Failed to update summary for approval {approval_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))
        
        @self.app.post("/api/approvals/{approval_id}/update-inline-comment")
        async def update_inline_comment(approval_id: int, request: Request):
            """Update a specific inline comment for a pending approval."""
            try:
                body = await request.json()
                comment_index = body.get('index')
                new_message = body.get('message', '')
                
                if comment_index is None:
                    raise HTTPException(status_code=400, detail="Comment index is required")
                
                success = await self.database.update_approval_inline_comment(
                    approval_id, comment_index, new_message
                )
                
                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval or comment not found")
                    
            except Exception as e:
                logger.error(f"Failed to update inline comment for approval {approval_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))
        
        @self.app.post("/api/approvals/{approval_id}/delete-comment")
        async def delete_approval_comment(approval_id: int):
            """Delete the review comment for a pending approval."""
            try:
                success = await self.database.delete_approval_comment(approval_id)
                
                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval not found")
                    
            except Exception as e:
                logger.error(f"Failed to delete comment for approval {approval_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))
        
        @self.app.post("/api/approvals/{approval_id}/delete-summary")
        async def delete_approval_summary(approval_id: int):
            """Delete the review summary for a pending approval."""
            try:
                success = await self.database.delete_approval_summary(approval_id)
                
                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval not found")
                    
            except Exception as e:
                logger.error(f"Failed to delete summary for approval {approval_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))
        
        @self.app.post("/api/approvals/{approval_id}/delete-inline-comment")
        async def delete_inline_comment(approval_id: int, request: Request):
            """Delete a specific inline comment for a pending approval."""
            try:
                body = await request.json()
                comment_index = body.get('index')
                
                if comment_index is None:
                    raise HTTPException(status_code=400, detail="Comment index is required")
                
                success = await self.database.delete_approval_inline_comment(
                    approval_id, comment_index
                )
                
                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval or comment not found")
                    
            except Exception as e:
                logger.error(f"Failed to delete inline comment for approval {approval_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))
    
    async def _post_github_review(self, pr_info: PRInfo, review_result: ReviewResult) -> bool:
        """Post a review to GitHub."""
        try:
            if review_result.action == ReviewAction.APPROVE_WITH_COMMENT:
                # Convert inline comments to GitHub format
                github_comments = [
                    {
                        'path': comment.file,
                        'line': comment.line,
                        'body': comment.message
                    }
                    for comment in review_result.comments
                ] if review_result.comments else None
                
                success = await self.github_client.approve_pr(
                    [pr_info.owner, pr_info.repo],
                    pr_info.number,
                    review_result.comment,
                    github_comments
                )
            elif review_result.action == ReviewAction.APPROVE_WITHOUT_COMMENT:
                # Convert inline comments to GitHub format even for approve without comment
                github_comments = [
                    {
                        'path': comment.file,
                        'line': comment.line,
                        'body': comment.message
                    }
                    for comment in review_result.comments
                ] if review_result.comments else None
                
                success = await self.github_client.approve_pr(
                    [pr_info.owner, pr_info.repo],
                    pr_info.number,
                    None,
                    github_comments
                )
            elif review_result.action == ReviewAction.REQUEST_CHANGES:
                # Convert inline comments to GitHub format
                github_comments = [
                    {
                        'path': comment.file,
                        'line': comment.line,
                        'body': comment.message
                    }
                    for comment in review_result.comments
                ]
                
                success = await self.github_client.request_changes(
                    [pr_info.owner, pr_info.repo],
                    pr_info.number,
                    github_comments,
                    review_result.summary or "Changes requested"
                )
            else:
                logger.warning(f"Unsupported review action for GitHub posting: {review_result.action}")
                return False
                
            return success
            
        except Exception as e:
            logger.error(f"Failed to post GitHub review: {e}")
            return False

    def get_app(self) -> FastAPI:
        """Get the FastAPI application instance."""
        return self.app