"""本地知识库（RAG）：加载 → 分块 → 本地 embedding → 向量库 → 检索，并包成工具。"""

from __future__ import annotations

import logging
from pathlib import Path

from langchain.tools import tool
from langchain_community.document_loaders import TextLoader
from langchain_core.tools import BaseTool
from langchain_huggingface import HuggingFaceEmbeddings
from langchain_text_splitters import RecursiveCharacterTextSplitter

logger = logging.getLogger("agent.knowledge")


def build_retriever(*, knowledge_file: Path, embedding_model: str, top_k: int):
    """加载本地文档、切分、嵌入，返回检索器（与原实现逐行对应）。"""
    logger.info("加载知识库: %s", knowledge_file)
    loader = TextLoader(str(knowledge_file), encoding="utf-8")
    documents = loader.load()

    splitter = RecursiveCharacterTextSplitter(chunk_size=500, chunk_overlap=50)
    chunks = splitter.split_documents(documents)
    logger.info("知识库切分为 %d 个块", len(chunks))

    logger.info("加载本地 embedding 模型: %s（首次运行需要下载）", embedding_model)
    embeddings = HuggingFaceEmbeddings(model_name=embedding_model)

    # 延迟导入：Chroma 只在真正建库时才需要，方便没装向量库时的静态检查
    from langchain_chroma import Chroma

    vectorstore = Chroma.from_documents(documents=chunks, embedding=embeddings)
    return vectorstore.as_retriever(search_kwargs={"k": top_k})


def build_knowledge_tool(retriever) -> BaseTool:
    """把检索器包成一个工具（工具描述直接决定模型何时调用它，别乱改文案）。"""

    # 知识库内容按个人经历自行修改：knowledge/my_knowledge.txt（仓库里给的是示例）
    @tool
    def search_knowledge(query: str) -> str:
        """当用户询问关于我的个人信息、项目经历、技能或简历细节时, 调用此工具检索本地知识"""
        matched_docs = retriever.invoke(query)
        return "\n\n".join(doc.page_content for doc in matched_docs)

    return search_knowledge
