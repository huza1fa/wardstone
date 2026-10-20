"""Rebuild synthetic fixtures with reportlab and pypdf; no real vendor data."""
from pathlib import Path
from reportlab.pdfgen import canvas
from reportlab.lib.utils import ImageReader
from PIL import Image, ImageDraw
from pypdf import PdfReader, PdfWriter

root = Path(__file__).parent

def build(name, pages):
    c = canvas.Canvas(str(root / name), pagesize=(612, 792), invariant=1)
    c.setTitle('Synthetic Wardstone test evidence')
    for number, draw in enumerate(pages, 1):
        draw(c)
        c.setFont('Helvetica', 10)
        # Scanned pages deliberately have no text layer, including the footer.
        if name != 'scanned.pdf' or number == 1:
            c.drawString(54, 40, f'SYNTHETIC TEST FIXTURE - Page {number}')
        c.showPage()
    c.save()

def text_page(title, text):
    def draw(c):
        c.setFont('Helvetica-Bold', 18)
        c.drawString(54, 730, title)
        c.setFont('Helvetica', 11)
        c.drawString(54, 690, text)
    return draw

identity = text_page('Identity controls', 'Synthetic Acme report: SSO and MFA are required for administrators.')
assurance = text_page('Independent assurance', 'Synthetic auditor: the scoped identity controls operated effectively.')
build('readable.pdf', [identity, assurance])
image = Image.new('RGB', (1000, 500), 'white')
ImageDraw.Draw(image).text((40, 60), 'SCANNED TEST PAGE: this content needs OCR.', fill='black', font_size=32)
build('scanned.pdf', [identity, lambda c: c.drawImage(ImageReader(image), 54, 400, width=504, height=252)])
build('over-pages.pdf', [identity] * 21)
def dense(c):
    c.setFont('Helvetica', 4)
    for row in range(140):
        c.drawString(30, 760-row*5, 'Synthetic repeated evidence for excerpt budget testing. ' * 5)
build('dense.pdf', [dense, assurance])
writer = PdfWriter()
writer.append(PdfReader(root / 'readable.pdf'))
writer.encrypt('synthetic-fixture-password')
with (root / 'encrypted.pdf').open('wb') as f:
    writer.write(f)
