"""Web UI server for managing PR reviews and approvals."""

import asyncio
import json
import logging
from pathlib import Path
from typing import Dict, Any, Optional, List

from fastapi import FastAPI, HTTPException, Request, Form
from fastapi.responses import HTMLResponse, JSONResponse
from fastapi.staticfiles import StaticFiles
from fastapi.templating import Jinja2Templates

from .database import (
    ReviewDatabase,
    PENDING_APPROVAL_STATUS_APPROVED,
    PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED,
    PENDING_APPROVAL_STATUS_PENDING,
    PENDING_APPROVAL_STATUS_REJECTED,
    PENDING_APPROVAL_STATUS_EXPIRED,
)
from .github_client import GitHubClient
from .models import ReviewRecord, ReviewResult, ReviewAction, InlineComment, PRInfo


logger = logging.getLogger(__name__)


class ReviewWebServer:
    """Web server for PR review management."""

    def __init__(self, database: ReviewDatabase, github_client: GitHubClient, sound_notifier=None, llm_integration=None):
        self.database = database
        self.github_client = github_client
        self.sound_notifier = sound_notifier
        self.llm_integration = llm_integration
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

    @staticmethod
    def _append_text_block(original: Optional[str], addition: str) -> str:
        """Append a formatted block of text with spacing."""
        addition_text = (addition or '').strip()
        if not addition_text:
            return (original or '').strip()

        original_text = (original or '').strip()
        if not original_text:
            return addition_text

        return f"{original_text}\n\n{addition_text}"
    
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
        .history-section { margin: 15px 0; }
        .history-title { font-weight: bold; color: #495057; margin-bottom: 8px; }
        .comparison-container { display: grid; grid-template-columns: 1fr 1fr; gap: 15px; margin: 10px 0; }
        .original-content, .final-content { padding: 10px; border-radius: 4px; }
        .original-content { background: #f8f9fa; border-left: 3px solid #6c757d; }
        .final-content { background: #d1ecf1; border-left: 3px solid #bee5eb; }
        .section-header { font-weight: bold; margin-bottom: 5px; color: #495057; }
        .no-content { font-style: italic; color: #6c757d; }
        .status-badge { display: inline-block; padding: 3px 8px; border-radius: 4px; font-size: 12px; font-weight: bold; }
        .status-approved { background: #d4edda; color: #155724; }
        .status-rejected { background: #f8d7da; color: #721c24; }
        .status-merged-closed { background: #e2e3e5; color: #495057; }
        .status-expired { background: #fff3cd; color: #8a6d3b; }
        .buttons { margin-top: 15px; }
        .btn { padding: 8px 16px; border: none; border-radius: 4px; cursor: pointer; margin-right: 10px; }
        .btn-approve { background: #28a745; color: white; }
        .btn-reject { background: #dc3545; color: white; }
        .btn-view { background: #17a2b8; color: white; }
        .btn-retry { background: #6c757d; color: white; }
        .btn-retry:hover { background: #5a6268; }
        .toast { position: fixed; bottom: 20px; right: 20px; background: #333; color: white; padding: 15px 25px; border-radius: 8px; z-index: 9999; display: none; animation: fadeIn 0.3s; }
        .toast.show { display: block; }
        @keyframes fadeIn { from { opacity: 0; transform: translateY(20px); } to { opacity: 1; transform: translateY(0); } }
        .tabs { border-bottom: 1px solid #ddd; margin-bottom: 20px; }
        .tab { display: inline-block; padding: 10px 20px; cursor: pointer; border-bottom: 2px solid transparent; }
        .tab.active { border-bottom-color: #007bff; color: #007bff; }
        .tab-content { display: none; }
        .tab-content.active { display: block; }
        .merged-closed-note { background: #f1f3f5; padding: 10px; border-left: 3px solid #adb5bd; margin: 10px 0; border-radius: 4px; color: #495057; }
        .expired-note { background: #fff8e1; padding: 10px; border-left: 3px solid #ffc107; margin: 10px 0; border-radius: 4px; color: #8a6d3b; }
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
            <div class="tab" onclick="showTab('approved')">Approved History</div>
            <div class="tab" onclick="showTab('rejected')">Rejected History</div>
            <div class="tab" onclick="showTab('merged-closed')">Merged/Closed</div>
            <div class="tab" onclick="showTab('expired')">Expired</div>
            <div class="tab" onclick="showTab('completed-reviews')">Completed Reviews</div>
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

        <div id="approved" class="tab-content">
            <div class="section">
                <h2>Approved Approvals History</h2>
                <div id="approved-approvals"></div>
            </div>
        </div>

        <div id="rejected" class="tab-content">
            <div class="section">
                <h2>Rejected Approvals History</h2>
                <div id="rejected-approvals"></div>
            </div>
        </div>

        <div id="merged-closed" class="tab-content">
            <div class="section">
                <h2>Merged or Closed Pending Approvals</h2>
                <div id="merged-closed-approvals"></div>
            </div>
        </div>

        <div id="expired" class="tab-content">
            <div class="section">
                <h2>Expired Pending Approvals</h2>
                <div id="expired-approvals"></div>
            </div>
        </div>

        <div id="completed-reviews" class="tab-content">
            <div class="section">
                <h2>Last 50 Completed PR Reviews</h2>
                <div id="completed-reviews-list"></div>
            </div>
        </div>
    </div>

    <!-- Toast notification -->
    <div id="toast" class="toast"></div>

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

        function refreshCurrentView() {
            // Find the currently active tab and refresh its data
            const activeTab = document.querySelector('.tab.active');
            if (activeTab) {
                const activeTabContent = document.querySelector('.tab-content.active');
                if (activeTabContent) {
                    const tabId = activeTabContent.id;
                    switch (tabId) {
                        case 'pending':
                            loadPendingApprovals();
                            break;
                        case 'human-review':
                            loadHumanReviews();
                            break;
                        case 'approved':
                            loadApprovedHistory();
                            break;
                        case 'rejected':
                            loadRejectedHistory();
                            break;
                        case 'merged-closed':
                            loadMergedClosedApprovals();
                            break;
                        case 'expired':
                            loadExpiredApprovals();
                            break;
                        case 'completed-reviews':
                            loadCompletedReviews();
                            break;
                    }
                }
            }
        }

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
            } else if (tabName === 'approved') {
                loadApprovedHistory();
            } else if (tabName === 'rejected') {
                loadRejectedHistory();
            } else if (tabName === 'merged-closed') {
                loadMergedClosedApprovals();
            } else if (tabName === 'expired') {
                loadExpiredApprovals();
            } else if (tabName === 'completed-reviews') {
                loadCompletedReviews();
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
                        <div class="pr-title">${approval.pr_title}
                            ${getActionBadge(approval.review_action)}
                        </div>
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
                                ${approval.inline_comments.map((comment, index) => `
                                    <div class="inline-comment" id="inline-comment-section-${approval.id}-${index}">
                                        <strong>${comment.file}:${comment.line}</strong>
                                        <button class="btn-small edit-btn" onclick="editInlineComment(${approval.id}, ${index})" style="margin-left: 10px; font-size: 12px; padding: 2px 6px;">Edit</button>
                                        <button class="btn-small delete-btn" onclick="deleteInlineComment(${approval.id}, ${index})" style="margin-left: 5px; font-size: 12px; padding: 2px 6px; background: #dc3545;">Delete</button>
                                        <br>
                                        <div id="inline-comment-display-${approval.id}-${index}" class="inline-comment-display">${comment.message}</div>
                                        <textarea id="inline-comment-edit-${approval.id}-${index}" class="inline-comment-editor" style="display: none; width: 100%; height: 80px; margin: 10px 0;">${comment.message}</textarea>
                                        <div id="inline-comment-edit-buttons-${approval.id}-${index}" style="display: none; margin-top: 5px;">
                                            <button class="btn btn-approve" onclick="saveInlineComment(${approval.id}, ${index})" style="margin-right: 5px;">Save</button>
                                            <button class="btn" onclick="cancelEditInlineComment(${approval.id}, ${index})">Cancel</button>
                                        </div>
                                    </div>
                                `).join('')}
                            </div>
                        ` : ''}
                        <div class="buttons">
                            <button class="btn btn-approve" onclick="approveDirectly(${approval.id})">
                                Approve & Post Review
                            </button>
                            <button class="btn btn-reject" onclick="rejectPR(${approval.id})">Reject</button>
                            <button class="btn btn-retry" onclick="reviewAgain(${approval.id})">Review Again</button>
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
                    refreshCurrentView();
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
                    refreshCurrentView();
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
                    refreshCurrentView();
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

        // Inline comment editing functions
        function editInlineComment(approvalId, commentIndex) {
            document.getElementById(`inline-comment-display-${approvalId}-${commentIndex}`).style.display = 'none';
            document.getElementById(`inline-comment-edit-${approvalId}-${commentIndex}`).style.display = 'block';
            document.getElementById(`inline-comment-edit-buttons-${approvalId}-${commentIndex}`).style.display = 'block';
        }

        function cancelEditInlineComment(approvalId, commentIndex) {
            document.getElementById(`inline-comment-display-${approvalId}-${commentIndex}`).style.display = 'block';
            document.getElementById(`inline-comment-edit-${approvalId}-${commentIndex}`).style.display = 'none';
            document.getElementById(`inline-comment-edit-buttons-${approvalId}-${commentIndex}`).style.display = 'none';
            // Reset textarea to original value
            const displayText = document.getElementById(`inline-comment-display-${approvalId}-${commentIndex}`).textContent;
            document.getElementById(`inline-comment-edit-${approvalId}-${commentIndex}`).value = displayText;
        }

        async function saveInlineComment(approvalId, commentIndex) {
            const newMessage = document.getElementById(`inline-comment-edit-${approvalId}-${commentIndex}`).value;
            try {
                const response = await fetch(`/api/approvals/${approvalId}/update-inline-comment`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ index: commentIndex, message: newMessage })
                });
                
                if (response.ok) {
                    document.getElementById(`inline-comment-display-${approvalId}-${commentIndex}`).textContent = newMessage;
                    cancelEditInlineComment(approvalId, commentIndex);
                } else {
                    alert('Failed to save inline comment');
                }
            } catch (error) {
                console.error('Failed to save inline comment:', error);
                alert('Failed to save inline comment');
            }
        }

        async function deleteInlineComment(approvalId, commentIndex) {
            if (!confirm('Delete this inline comment? It will be removed from the review.')) return;
            
            try {
                const response = await fetch(`/api/approvals/${approvalId}/delete-inline-comment`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ index: commentIndex })
                });
                
                if (response.ok) {
                    // Refresh the current view to update counts and indices
                    refreshCurrentView();
                } else {
                    alert('Failed to delete inline comment');
                }
            } catch (error) {
                console.error('Failed to delete inline comment:', error);
                alert('Failed to delete inline comment');
            }
        }

        async function approveDirectly(id) {
            if (!confirm('Approve and post this review to GitHub?')) return;

            // Don't pass comment from DOM - let server use database edited versions
            try {
                const response = await fetch(`/api/approvals/${id}/approve`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({})
                });

                if (response.ok) {
                    refreshCurrentView();
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

        function showToast(message, duration = 3000) {
            const toast = document.getElementById('toast');
            toast.textContent = message;
            toast.classList.add('show');
            setTimeout(() => {
                toast.classList.remove('show');
            }, duration);
        }

        async function reviewAgain(id) {
            if (!confirm('Discard this review and trigger a fresh one?')) return;

            try {
                const response = await fetch(`/api/approvals/${id}/review-again`, {
                    method: 'POST'
                });

                if (response.ok) {
                    const result = await response.json();
                    showToast(result.message || 'Review triggered. Refresh to see results.');
                    refreshCurrentView();
                } else {
                    const errorText = await response.text();
                    alert('Failed to trigger re-review: ' + errorText);
                }
            } catch (error) {
                console.error('Failed to trigger re-review:', error);
                alert('Failed to trigger re-review: ' + error.message);
            }
        }

        async function loadApprovedHistory() {
            try {
                const response = await fetch('/api/approved-approvals');
                const approvals = await response.json();
                const container = document.getElementById('approved-approvals');
                
                if (approvals.length === 0) {
                    container.innerHTML = '<p>No approved approvals found.</p>';
                    return;
                }
                
                container.innerHTML = approvals.map(approval => {
                    const originalComment = approval.original_review_comment || '';
                    const finalComment = approval.final_review_comment || '';
                    const originalSummary = approval.original_review_summary || '';
                    const finalSummary = approval.final_review_summary || '';
                    const originalInlineComments = approval.original_inline_comments || [];
                    const finalInlineComments = approval.inline_comments || [];
                    
                    return `
                        <div class="pr-card">
                            <div class="pr-title">${approval.pr_title}
                                <span class="status-badge status-approved">APPROVED</span>
                            </div>
                            <div class="pr-meta">
                                ${approval.repository} #${approval.pr_number} by ${approval.pr_author}
                                <br><small>Created: ${new Date(approval.created_at).toLocaleString()}</small>
                                <br><small>Action: ${approval.review_action.replace('_', ' ').toUpperCase()}</small>
                            </div>
                            
                            ${(originalComment || finalComment) ? `
                                <div class="history-section">
                                    <div class="history-title">Review Comments</div>
                                    <div class="comparison-container">
                                        <div class="original-content">
                                            <div class="section-header">Original</div>
                                            <div class="content">${originalComment || '<span class="no-content">No comment</span>'}</div>
                                        </div>
                                        <div class="final-content">
                                            <div class="section-header">Final (Posted to GitHub)</div>
                                            <div class="content">${finalComment || '<span class="no-content">No comment</span>'}</div>
                                        </div>
                                    </div>
                                </div>
                            ` : ''}
                            
                            ${(originalSummary || finalSummary) ? `
                                <div class="history-section">
                                    <div class="history-title">Review Summary</div>
                                    <div class="comparison-container">
                                        <div class="original-content">
                                            <div class="section-header">Original</div>
                                            <div class="content">${originalSummary || '<span class="no-content">No summary</span>'}</div>
                                        </div>
                                        <div class="final-content">
                                            <div class="section-header">Final (Posted to GitHub)</div>
                                            <div class="content">${finalSummary || '<span class="no-content">No summary</span>'}</div>
                                        </div>
                                    </div>
                                </div>
                            ` : ''}
                            
                            ${(originalInlineComments.length > 0 || finalInlineComments.length > 0) ? `
                                <div class="history-section">
                                    <div class="history-title">Inline Comments</div>
                                    <div class="comparison-container">
                                        <div class="original-content">
                                            <div class="section-header">Original (${originalInlineComments.length})</div>
                                            ${originalInlineComments.length > 0 ? 
                                                originalInlineComments.map(comment => `
                                                    <div class="inline-comment">
                                                        <strong>${comment.file}:${comment.line}</strong><br>
                                                        ${comment.message}
                                                    </div>
                                                `).join('') : 
                                                '<span class="no-content">No inline comments</span>'
                                            }
                                        </div>
                                        <div class="final-content">
                                            <div class="section-header">Final - Posted to GitHub (${finalInlineComments.length})</div>
                                            ${finalInlineComments.length > 0 ? 
                                                finalInlineComments.map(comment => `
                                                    <div class="inline-comment">
                                                        <strong>${comment.file}:${comment.line}</strong><br>
                                                        ${comment.message}
                                                    </div>
                                                `).join('') : 
                                                '<span class="no-content">No inline comments</span>'
                                            }
                                        </div>
                                    </div>
                                </div>
                            ` : ''}
                            
                            <div class="buttons">
                                <a href="${approval.pr_url}" target="_blank" class="btn btn-view">View PR</a>
                            </div>
                        </div>
                    `;
                }).join('');
            } catch (error) {
                console.error('Failed to load approved history:', error);
            }
        }

        async function loadRejectedHistory() {
            try {
                const response = await fetch('/api/rejected-approvals');
                const approvals = await response.json();
                const container = document.getElementById('rejected-approvals');
                
                if (approvals.length === 0) {
                    container.innerHTML = '<p>No rejected approvals found.</p>';
                    return;
                }
                
                container.innerHTML = approvals.map(approval => {
                    const originalComment = approval.original_review_comment || '';
                    const finalComment = approval.final_review_comment || '';
                    const originalSummary = approval.original_review_summary || '';
                    const finalSummary = approval.final_review_summary || '';
                    const originalInlineComments = approval.original_inline_comments || [];
                    const finalInlineComments = approval.inline_comments || [];
                    
                    return `
                        <div class="pr-card">
                            <div class="pr-title">${approval.pr_title}
                                <span class="status-badge status-rejected">REJECTED</span>
                            </div>
                            <div class="pr-meta">
                                ${approval.repository} #${approval.pr_number} by ${approval.pr_author}
                                <br><small>Created: ${new Date(approval.created_at).toLocaleString()}</small>
                                <br><small>Action: ${approval.review_action.replace('_', ' ').toUpperCase()}</small>
                                ${approval.review_comment ? `<br><small>Rejection Reason: ${approval.review_comment}</small>` : ''}
                            </div>
                            
                            ${(originalComment || finalComment) ? `
                                <div class="history-section">
                                    <div class="history-title">Review Comments (Not Posted)</div>
                                    <div class="comparison-container">
                                        <div class="original-content">
                                            <div class="section-header">Original</div>
                                            <div class="content">${originalComment || '<span class="no-content">No comment</span>'}</div>
                                        </div>
                                        <div class="final-content">
                                            <div class="section-header">Final (Not Posted - Rejected)</div>
                                            <div class="content">${finalComment || '<span class="no-content">No comment</span>'}</div>
                                        </div>
                                    </div>
                                </div>
                            ` : ''}
                            
                            ${(originalSummary || finalSummary) ? `
                                <div class="history-section">
                                    <div class="history-title">Review Summary (Not Posted)</div>
                                    <div class="comparison-container">
                                        <div class="original-content">
                                            <div class="section-header">Original</div>
                                            <div class="content">${originalSummary || '<span class="no-content">No summary</span>'}</div>
                                        </div>
                                        <div class="final-content">
                                            <div class="section-header">Final (Not Posted - Rejected)</div>
                                            <div class="content">${finalSummary || '<span class="no-content">No summary</span>'}</div>
                                        </div>
                                    </div>
                                </div>
                            ` : ''}
                            
                            ${(originalInlineComments.length > 0 || finalInlineComments.length > 0) ? `
                                <div class="history-section">
                                    <div class="history-title">Inline Comments (Not Posted)</div>
                                    <div class="comparison-container">
                                        <div class="original-content">
                                            <div class="section-header">Original (${originalInlineComments.length})</div>
                                            ${originalInlineComments.length > 0 ? 
                                                originalInlineComments.map(comment => `
                                                    <div class="inline-comment">
                                                        <strong>${comment.file}:${comment.line}</strong><br>
                                                        ${comment.message}
                                                    </div>
                                                `).join('') : 
                                                '<span class="no-content">No inline comments</span>'
                                            }
                                        </div>
                                        <div class="final-content">
                                            <div class="section-header">Final (Not Posted - Rejected) (${finalInlineComments.length})</div>
                                            ${finalInlineComments.length > 0 ? 
                                                finalInlineComments.map(comment => `
                                                    <div class="inline-comment">
                                                        <strong>${comment.file}:${comment.line}</strong><br>
                                                        ${comment.message}
                                                    </div>
                                                `).join('') : 
                                                '<span class="no-content">No inline comments</span>'
                                            }
                                        </div>
                                    </div>
                                </div>
                            ` : ''}
                            
                            <div class="buttons">
                                <a href="${approval.pr_url}" target="_blank" class="btn btn-view">View PR</a>
                            </div>
                        </div>
                    `;
                }).join('');
            } catch (error) {
                console.error('Failed to load rejected history:', error);
            }
        }

        async function loadMergedClosedApprovals() {
            try {
                const response = await fetch('/api/merged-or-closed-approvals');
                const approvals = await response.json();
                const container = document.getElementById('merged-closed-approvals');

                if (approvals.length === 0) {
                    container.innerHTML = '<p>No merged or closed approvals.</p>';
                    return;
                }

                container.innerHTML = approvals.map(approval => `
                    <div class="pr-card">
                        <div class="pr-title">${approval.pr_title}</div>
                        <div class="pr-meta">
                            ${approval.repository} #${approval.pr_number} by ${approval.pr_author}
                            <br><small>Originally queued: ${new Date(approval.created_at).toLocaleString()}</small>
                        </div>
                        <div class="merged-closed-note">
                            <span class="status-badge status-merged-closed">MERGED/CLOSED</span>
                            This pending approval was cleared because the pull request was merged or closed.
                        </div>
                        ${approval.display_review_comment ? `
                            <div class="review-comment">
                                <strong>Original Comment:</strong>
                                <div>${approval.display_review_comment}</div>
                            </div>` : ''}
                        ${approval.display_review_summary ? `
                            <div class="review-comment">
                                <strong>Original Summary:</strong>
                                <div>${approval.display_review_summary}</div>
                            </div>` : ''}
                        ${approval.inline_comments.length > 0 ? `
                            <div class="inline-comments">
                                <strong>Inline Comments (${approval.inline_comments.length}):</strong>
                                ${approval.inline_comments.map((comment) => `
                                    <div class="inline-comment">
                                        <strong>${comment.file}:${comment.line}</strong><br>
                                        ${comment.message}
                                    </div>
                                `).join('')}
                            </div>
                        ` : ''}
                        <div class="buttons">
                            <a href="${approval.pr_url}" target="_blank" class="btn btn-view">View PR</a>
                        </div>
                    </div>
                `).join('');
            } catch (error) {
                console.error('Failed to load merged/closed approvals:', error);
            }
        }

        async function loadExpiredApprovals() {
            try {
                const response = await fetch('/api/expired-approvals');
                const approvals = await response.json();
                const container = document.getElementById('expired-approvals');

                if (approvals.length === 0) {
                    container.innerHTML = '<p>No expired approvals.</p>';
                    return;
                }

                container.innerHTML = approvals.map(approval => `
                    <div class="pr-card">
                        <div class="pr-title">${approval.pr_title}</div>
                        <div class="pr-meta">
                            ${approval.repository} #${approval.pr_number} by ${approval.pr_author}
                            <br><small>Originally queued: ${new Date(approval.created_at).toLocaleString()}</small>
                        </div>
                        <div class="expired-note">
                            <span class="status-badge status-expired">EXPIRED</span>
                            This pending approval was replaced after a new commit arrived on the pull request.
                        </div>
                        ${approval.display_review_comment ? `
                            <div class="review-comment">
                                <strong>Original Comment:</strong>
                                <div>${approval.display_review_comment}</div>
                            </div>` : ''}
                        ${approval.display_review_summary ? `
                            <div class="review-comment">
                                <strong>Original Summary:</strong>
                                <div>${approval.display_review_summary}</div>
                            </div>` : ''}
                        ${approval.inline_comments.length > 0 ? `
                            <div class="inline-comments">
                                <strong>Inline Comments (${approval.inline_comments.length}):</strong>
                                ${approval.inline_comments.map((comment) => `
                                    <div class="inline-comment">
                                        <strong>${comment.file}:${comment.line}</strong><br>
                                        ${comment.message}
                                    </div>
                                `).join('')}
                            </div>
                        ` : ''}
                        <div class="buttons">
                            <a href="${approval.pr_url}" target="_blank" class="btn btn-view">View PR</a>
                        </div>
                    </div>
                `).join('');
            } catch (error) {
                console.error('Failed to load expired approvals:', error);
            }
        }

        async function loadCompletedReviews() {
            try {
                const response = await fetch('/api/completed-reviews');
                const reviews = await response.json();
                const container = document.getElementById('completed-reviews-list');
                
                if (reviews.length === 0) {
                    container.innerHTML = '<p>No completed reviews found.</p>';
                    return;
                }
                
                container.innerHTML = reviews.map(review => {
                    const actionBadge = getActionBadge(review.review_action);
                    
                    return `
                        <div class="pr-card">
                            <div class="pr-title">${review.pr_title}
                                ${actionBadge}
                            </div>
                            <div class="pr-meta">
                                ${review.repository} #${review.pr_number} by ${review.pr_author}
                                <br><small>Reviewed: ${new Date(review.reviewed_at).toLocaleString()}</small>
                                <br><small>Head SHA: ${review.head_sha ? review.head_sha.substring(0, 8) : 'N/A'}</small>
                                ${review.inline_comments_count > 0 ? `<br><small>Inline Comments: ${review.inline_comments_count}</small>` : ''}
                            </div>
                            
                            ${review.review_comment ? `
                                <div class="review-comment">
                                    <strong>Review Comment:</strong><br>
                                    ${review.review_comment}
                                </div>
                            ` : ''}
                            
                            ${review.review_summary ? `
                                <div class="review-comment">
                                    <strong>Review Summary:</strong><br>
                                    ${review.review_summary}
                                </div>
                            ` : ''}
                            
                            ${review.review_reason ? `
                                <div class="review-comment">
                                    <strong>Review Reason:</strong><br>
                                    ${review.review_reason}
                                </div>
                            ` : ''}
                            
                            <div class="buttons">
                                <a href="https://github.com/${review.repository}/pull/${review.pr_number}" target="_blank" class="btn btn-view">View PR</a>
                            </div>
                        </div>
                    `;
                }).join('');
            } catch (error) {
                console.error('Failed to load completed reviews:', error);
            }
        }

        function getActionBadge(action) {
            switch (action) {
                case 'approve_with_comment':
                    return '<span class="status-badge status-approved">APPROVED WITH COMMENT</span>';
                case 'approve_without_comment':
                    return '<span class="status-badge status-approved">APPROVED</span>';
                case 'request_changes':
                    return '<span class="status-badge status-rejected">CHANGES REQUESTED</span>';
                case 'requires_human_review':
                    return '<span class="status-badge" style="background: #fff3cd; color: #856404;">HUMAN REVIEW</span>';
                default:
                    return `<span class="status-badge" style="background: #e2e3e5; color: #6c757d;">${action.replace('_', ' ').toUpperCase()}</span>`;
            }
        }

        // Load initial data
        loadPendingApprovals();
    </script>
</body>
</html>
"""
        
        dashboard_path = self.templates_dir / "dashboard.html"
        # Always write the template to ensure we have the latest version
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
                
                if approval['status'] != PENDING_APPROVAL_STATUS_PENDING:
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
                # If user_comment is provided from modal, use it; otherwise use display_review_comment (which prioritizes edited)
                final_comment = user_comment if user_comment else approval['display_review_comment']
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
                        approval_id,
                        PENDING_APPROVAL_STATUS_APPROVED,
                        review_result.comment
                    )
                    
                    # Record the review in main reviews table
                    await self.database.record_review(pr_info, review_result)
                    
                    # Play approval sound
                    if self.sound_notifier:
                        await self.sound_notifier.play_approval_sound()
                    
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
                    approval_id, PENDING_APPROVAL_STATUS_REJECTED, reason
                )

                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval not found")

            except Exception as e:
                logger.error(f"Failed to reject PR {approval_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.post("/api/approvals/{approval_id}/review-again")
        async def review_again(approval_id: int):
            """Expire pending approval and trigger fresh review."""
            try:
                if not self.llm_integration:
                    raise HTTPException(
                        status_code=500,
                        detail="LLM integration not available"
                    )

                # Get the pending approval
                approval = await self.database.get_pending_approval(approval_id)
                if not approval:
                    raise HTTPException(status_code=404, detail="Approval not found")

                if approval['status'] != PENDING_APPROVAL_STATUS_PENDING:
                    raise HTTPException(
                        status_code=400,
                        detail="Only pending approvals can be re-reviewed"
                    )

                # Mark as expired (keeps audit trail)
                await self.database.update_pending_approval_status(
                    approval_id,
                    PENDING_APPROVAL_STATUS_EXPIRED,
                    "Re-review requested by user"
                )

                # Delete the review record to allow re-review
                await self.database.delete_review_for_re_review(
                    approval['repository'],
                    approval['pr_number'],
                    approval['head_sha']
                )

                # Build PRInfo for re-review
                pr_info = PRInfo(
                    id=approval['pr_number'],
                    number=approval['pr_number'],
                    repository=approval['repository'].split('/'),
                    url=approval['pr_url'],
                    title=approval['pr_title'],
                    author=approval['pr_author'],
                    head_sha=approval['head_sha'],
                    base_sha=approval['base_sha'],
                )

                # Trigger fresh review in background
                async def run_review():
                    try:
                        logger.info(
                            f"Starting re-review for {pr_info.repository_name}#{pr_info.number}"
                        )
                        review_result = await self.llm_integration.review_pr(pr_info)
                        logger.info(
                            f"Re-review complete for {pr_info.repository_name}#{pr_info.number}: "
                            f"{review_result.action.value}"
                        )
                        # Create new pending approval if needed
                        if review_result.action in (
                            ReviewAction.APPROVE_WITH_COMMENT,
                            ReviewAction.APPROVE_WITHOUT_COMMENT,
                        ):
                            await self.database.create_pending_approval(pr_info, review_result)
                            if self.sound_notifier:
                                await self.sound_notifier.play_notification()
                        elif review_result.action == ReviewAction.REQUIRES_HUMAN_REVIEW:
                            await self.database.record_review(pr_info, review_result)
                            if self.sound_notifier:
                                await self.sound_notifier.play_notification()
                        else:
                            # REQUEST_CHANGES or other actions
                            await self.database.record_review(pr_info, review_result)
                    except Exception as e:
                        logger.error(f"Re-review failed for {pr_info.repository_name}#{pr_info.number}: {e}")

                asyncio.create_task(run_review())

                logger.info(f"Triggered re-review for approval {approval_id}")
                return JSONResponse(content={
                    "status": "success",
                    "message": "Review triggered. Refresh in a moment to see results."
                })

            except HTTPException:
                raise
            except Exception as e:
                logger.error(f"Failed to trigger re-review for approval {approval_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/approved-approvals")
        async def get_approved_approvals():
            """Get approved approvals history."""
            try:
                approvals = await self.database.get_approved_approvals()
                return JSONResponse(content=approvals)
            except Exception as e:
                logger.error(f"Failed to get approved approvals: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/merged-or-closed-approvals")
        async def get_merged_or_closed_approvals():
            """Get approvals cleared because PRs were merged or closed."""
            try:
                approvals = await self.database.get_pending_approvals(
                    PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED
                )
                return JSONResponse(content=approvals)
            except Exception as e:
                logger.error(f"Failed to get merged/closed approvals: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/expired-approvals")
        async def get_expired_approvals():
            """Get approvals that were superseded by new commits."""
            try:
                approvals = await self.database.get_pending_approvals(
                    PENDING_APPROVAL_STATUS_EXPIRED
                )
                return JSONResponse(content=approvals)
            except Exception as e:
                logger.error(f"Failed to get expired approvals: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/outdated-approvals")
        async def get_outdated_approvals_legacy():
            """Deprecated alias for merged-or-closed approvals."""
            logger.warning("/api/outdated-approvals is deprecated; use /api/merged-or-closed-approvals instead")
            try:
                approvals = await self.database.get_pending_approvals(
                    PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED
                )
                return JSONResponse(content=approvals)
            except Exception as e:
                logger.error(f"Failed to get merged/closed approvals via legacy route: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/rejected-approvals")
        async def get_rejected_approvals():
            """Get rejected approvals history."""
            try:
                approvals = await self.database.get_rejected_approvals()
                return JSONResponse(content=approvals)
            except Exception as e:
                logger.error(f"Failed to get rejected approvals: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/completed-reviews")
        async def get_completed_reviews():
            """Get completed PR reviews."""
            try:
                reviews = await self.database.get_completed_reviews()
                return JSONResponse(content=[{
                    "id": review.id,
                    "repository": review.repository,
                    "pr_number": review.pr_number,
                    "pr_title": review.pr_title,
                    "pr_author": review.pr_author,
                    "review_action": review.review_action.value,
                    "review_reason": review.review_reason,
                    "review_comment": review.review_comment,
                    "review_summary": review.review_summary,
                    "inline_comments_count": review.inline_comments_count,
                    "reviewed_at": review.reviewed_at,
                    "head_sha": review.head_sha,
                    "base_sha": review.base_sha
                } for review in reviews])
            except Exception as e:
                logger.error(f"Failed to get completed reviews: {e}")
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
            inline_comments_payload = None
            dropped_comments: List[InlineComment] = []

            if review_result.comments:
                payload, dropped = await self.github_client.prepare_inline_comments(
                    pr_info.repository,
                    pr_info.number,
                    review_result.comments,
                )
                inline_comments_payload = payload or None
                review_result.comments = [
                    InlineComment(file=item['path'], line=item['line'], message=item['body'])
                    for item in payload
                ]
                dropped_comments = dropped
                logger.info(
                    "Prepared %s inline comments, dropped %s for %s/%s#%s",
                    len(payload),
                    len(dropped_comments),
                    pr_info.repository[0],
                    pr_info.repository[1],
                    pr_info.number,
                )
            else:
                review_result.comments = []

            if dropped_comments:
                fallback_text = self.github_client.format_dropped_inline_comments(dropped_comments)
                if fallback_text:
                    if review_result.action == ReviewAction.REQUEST_CHANGES:
                        review_result.summary = self._append_text_block(review_result.summary, fallback_text)
                    else:
                        review_result.comment = self._append_text_block(review_result.comment, fallback_text)

            if review_result.action == ReviewAction.APPROVE_WITH_COMMENT:
                success = await self.github_client.approve_pr(
                    pr_info.repository,
                    pr_info.number,
                    review_result.comment,
                    inline_comments_payload,
                )
            elif review_result.action == ReviewAction.APPROVE_WITHOUT_COMMENT:
                body = review_result.comment if review_result.comment else None
                success = await self.github_client.approve_pr(
                    pr_info.repository,
                    pr_info.number,
                    body,
                    inline_comments_payload,
                )
            elif review_result.action == ReviewAction.REQUEST_CHANGES:
                success = await self.github_client.request_changes(
                    pr_info.repository,
                    pr_info.number,
                    inline_comments_payload or [],
                    review_result.summary or "Changes requested",
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
